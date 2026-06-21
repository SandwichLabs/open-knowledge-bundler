package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/applog"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/agent"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/extract"
	"github.com/spf13/cobra"
)

var (
	extractCorpus     string
	extractOut        string
	extractBootstrap  bool
	extractSampleN    int
	extractChunkChars int
	extractOverlap    int
	extractGlean      int
	extractMaxChars   int
	extractResolve    bool
	extractThreshold  float64
	extractGrayLo     float64
	extractMaxAdjud   int
	extractTier       string
	extractModel      string
	extractProcessor  string
	extractIngest     bool
	extractYes        bool
	extractTime       string
	extractMaxTokens  int
)

var extractCmd = &cobra.Command{
	Use:   "extract",
	Short: "Build a knowledge graph from a corpus with a local LLM",
	Long: `Turns a prose corpus into a resolved knowledge graph, fully in-process
(no external LLM server). Five stages: ontology bootstrap -> grammar-constrained
extraction -> gleaning (recall) -> entity resolution -> relation normalization,
then emits the cbi ingest format (nodes.ndjson, edges.ndjson, domain.yaml,
vocab.txt) and can ingest it directly.

The ontology (entity types + a closed, directional relation vocabulary) lives in
the domain.yaml ` + "`ontology:`" + ` block. On first run (or with --bootstrap) it is
proposed from a corpus sample; without --bootstrap the run stops so you can
review/edit it, then re-run.

Examples:
  cbi extract --corpus medical.json -o med-graph/ --bootstrap --glean 1 --resolve --ingest
  cbi extract --corpus docs/ --config domain.yaml -o out/ --tier xl`,
	RunE: runExtract,
}

func runExtract(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	start := time.Now()

	configPath := cfgFile
	if configPath == "" {
		configPath = "domain.yaml"
	}
	ts, err := time.Parse("2006-01-02", extractTime)
	if err != nil {
		return fmt.Errorf("parsing --time: %w", err)
	}

	// 1. Load (or start) the domain config.
	cfg, err := extract.LoadConfig(configPath)
	if err != nil {
		return err
	}

	// 2. Load + chunk the corpus.
	corpus, err := extract.LoadCorpus(extractCorpus)
	if err != nil {
		return fmt.Errorf("loading corpus: %w", err)
	}
	if extractMaxChars > 0 && len(corpus) > extractMaxChars {
		corpus = corpus[:extractMaxChars]
	}
	chunks := extract.ChunkText(corpus, extractChunkChars, extractOverlap)
	fmt.Fprintf(os.Stderr, "corpus %d chars -> %d chunks\n", len(corpus), len(chunks))

	// 3. Resolve model sources + processor backend (reuse the agent's config).
	acfg, err := agent.LoadConfig(false, false)
	if err != nil {
		return fmt.Errorf("loading model config: %w", err)
	}
	if extractProcessor != "" {
		acfg.Processor = extractProcessor
	}
	llmSource := extractModel
	if llmSource == "" {
		if err := acfg.SetTier(extractTier); err != nil {
			return err
		}
		llmSource = acfg.LLMSource()
	}
	if acfg.Processor != "" {
		if err := os.Setenv("KRONK_PROCESSOR", acfg.Processor); err != nil {
			return fmt.Errorf("setting KRONK_PROCESSOR: %w", err)
		}
		fmt.Fprintf(os.Stderr, "llama.cpp backend: %s\n", acfg.Processor)
	}

	// 4. Load the generation model.
	fmt.Fprintf(os.Stderr, "loading generation model %s ...\n", llmSource)
	gen, err := extract.NewGenerator(ctx, llmSource, extractMaxTokens, applog.FmtLogger)
	if err != nil {
		return err
	}
	defer gen.Close()

	progress := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }

	// Stage 0 — ontology bootstrap (if missing or requested).
	autoBootstrap := cfg.Ontology == nil || len(cfg.Ontology.Relations) == 0
	if extractBootstrap || autoBootstrap {
		fmt.Fprintln(os.Stderr, "bootstrapping ontology from a corpus sample ...")
		ont, err := extract.Bootstrap(ctx, gen, chunks, extractSampleN)
		if err != nil {
			return err
		}
		cfg.Ontology = ont
		if err := extract.SaveConfig(configPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "ontology: %d entity types, %d relations -> %s\n",
			len(ont.EntityTypes), len(ont.Relations), configPath)

		// "Bootstrap, then editable": stop after an automatic bootstrap so the
		// user can review/edit, unless they explicitly asked to bootstrap-and-run
		// (--bootstrap) or pass --yes.
		if autoBootstrap && !extractBootstrap && !extractYes {
			fmt.Fprintf(os.Stderr, "\nReview and edit the ontology in %s, then re-run to extract (or pass --yes to continue now).\n", configPath)
			return nil
		}
	}
	ont := cfg.Ontology

	// 5. Embedder (needed for resolution and for --ingest). Loaded only if used.
	var emb extract.Embedder
	if extractResolve || extractIngest {
		dim := cfg.EmbeddingDim
		if dim == 0 {
			dim = 768
		}
		fmt.Fprintf(os.Stderr, "loading embedding model %s ...\n", acfg.EmbedSource)
		e, err := agent.NewEmbedder(ctx, acfg.EmbedSource, dim, applog.FmtLogger)
		if err != nil {
			return fmt.Errorf("loading embedder: %w", err)
		}
		defer e.Close()
		emb = e
	}

	// Stage 1–2 — extract + glean.
	fmt.Fprintln(os.Stderr, "extracting entities + relations ...")
	graph, err := extract.Extract(ctx, gen, ont, chunks, extractGlean, progress)
	if err != nil {
		return err
	}

	// Stage 3 — entity resolution (emb==nil => exact-merge only).
	if extractResolve {
		fmt.Fprintln(os.Stderr, "resolving entities ...")
	}
	resEmb := emb
	if !extractResolve {
		resEmb = nil
	}
	res, err := extract.Resolve(ctx, gen, resEmb, graph, extractThreshold, extractGrayLo, extractMaxAdjud, progress)
	if err != nil {
		return err
	}

	// Stage 4 — relation normalization + direction.
	rep, err := extract.Normalize(ctx, gen, ont, res, extractMaxAdjud, progress)
	if err != nil {
		return err
	}

	// Stage 5 — emit.
	dbName := cfg.DatabasePath
	if dbName == "" {
		dbName = cfg.DomainName + ".duckdb"
	}
	opts := extract.EmitOptions{
		DomainName:     cfg.DomainName,
		EmbeddingDim:   cfg.EmbeddingDim,
		EmbeddingModel: cfg.EmbeddingModel,
		EndpointURL:    cfg.EndpointURL,
		DatabasePath:   dbName,
	}
	if err := extract.Emit(extractOut, ont, res, opts); err != nil {
		return err
	}

	// Optional in-process ingest.
	if extractIngest {
		dim := cfg.EmbeddingDim
		if dim == 0 {
			dim = 768
		}
		dbPath := extractOut + "/" + dbName
		fmt.Fprintln(os.Stderr, "ingesting (in-process embeddings) ...")
		if err := extract.Ingest(ctx, dbPath, res, emb, dim, ts, progress); err != nil {
			return err
		}
	}

	// Report.
	fmt.Fprintf(os.Stderr, "\nDONE in %s\n", time.Since(start).Round(time.Second))
	fmt.Fprintf(os.Stderr, "  nodes: %d  edges: %d  types: %d\n", len(res.Nodes), len(res.Relations), len(extract.SortedTypeNames(res)))
	fmt.Fprintf(os.Stderr, "  relation vocab: %d  (clustered merges: %d, adjudications: %d)\n", len(rep.RelationVocab), res.Merged, res.Adjudicate)
	fmt.Fprintf(os.Stderr, "  normalize: %d inverted, %d flipped, %d off-vocab mapped, %d bucketed, %d type-mismatch\n",
		rep.Inverted, rep.Flipped, rep.OffVocabMapped, rep.OffVocabBucket, rep.TypeMismatch)
	fmt.Fprintf(os.Stderr, "  output: %s/  (LLM calls: %d)\n", extractOut, gen.Calls())
	return nil
}

func init() {
	f := extractCmd.Flags()
	f.StringVar(&extractCorpus, "corpus", "", "corpus file or directory (required)")
	f.StringVarP(&extractOut, "out", "o", "out", "output directory for the extracted graph")
	f.BoolVar(&extractBootstrap, "bootstrap", false, "(re)derive the ontology from a corpus sample and continue the run")
	f.IntVar(&extractSampleN, "sample-chunks", 8, "chunks sampled across the corpus for ontology bootstrap")
	f.IntVar(&extractChunkChars, "chunk-chars", 6000, "target characters per chunk")
	f.IntVar(&extractOverlap, "overlap", 400, "character overlap between chunks")
	f.IntVar(&extractGlean, "glean", 0, "extra recall passes per chunk (0 = off)")
	f.IntVar(&extractMaxChars, "max-chars", 0, "cap corpus characters (0 = all)")
	f.BoolVar(&extractResolve, "resolve", false, "enable embedding-cluster entity resolution (else exact-merge only)")
	f.Float64Var(&extractThreshold, "resolve-threshold", 0.86, "cosine >= this auto-merges entities of the same type")
	f.Float64Var(&extractGrayLo, "resolve-gray-lo", 0.80, "cosine in [gray-lo, threshold) is LLM-adjudicated")
	f.IntVar(&extractMaxAdjud, "max-adjudicate", 400, "cap on LLM adjudication/mapping calls (0 = unlimited)")
	f.StringVar(&extractTier, "tier", "large", "model size tier (small|medium|large|xl|moe)")
	f.StringVar(&extractModel, "model", "", "override the LLM with an explicit kronk model source")
	f.StringVar(&extractProcessor, "gpu", "", "llama.cpp backend (cpu|cuda|rocm|vulkan); overrides config")
	f.BoolVar(&extractIngest, "ingest", false, "ingest the result into DuckDB in-process (fully local)")
	f.BoolVar(&extractYes, "yes", false, "continue without stopping for ontology review after an auto-bootstrap")
	f.StringVar(&extractTime, "time", time.Now().Format("2006-01-02"), "ingestion timestamp (YYYY-MM-DD), with --ingest")
	f.IntVar(&extractMaxTokens, "max-tokens", 8192, "max output tokens per LLM call")
	_ = extractCmd.MarkFlagRequired("corpus")
	rootCmd.AddCommand(extractCmd)
}
