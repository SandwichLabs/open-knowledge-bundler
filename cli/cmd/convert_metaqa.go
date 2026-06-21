package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/eval"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/metaqa"
	"github.com/spf13/cobra"
)

var (
	mqSrc     string
	mqOut     string
	mqHops    []int
	mqSample  int
	mqSeed    int64
	mqSplit   string
	mqDBName  string
)

var convertCmd = &cobra.Command{
	Use:   "convert",
	Short: "Convert external datasets into the cbi ingest format",
}

var convertMetaQACmd = &cobra.Command{
	Use:   "metaqa",
	Short: "Convert the MetaQA dataset into a cbi domain + eval question set",
	Long: `Converts a local copy of MetaQA (the WikiMovies knowledge base plus the
1/2/3-hop QA sets) into a cbi-ingestable bundle:

  nodes.ndjson      pre-resolved Movie/Person/Year/Genre/... nodes
  edges.ndjson      DIRECTED_BY / STARRED / HAS_GENRE / ... relationships
  domain.yaml       cbi domain config for the movie graph
  vocab.txt         entity-name vocabulary for precision scoring (cbi eval --vocab)
  questions.jsonl   sampled QA answer key for cbi eval (tagged by hop)

MetaQA is distributed on Google Drive (see github.com/yuyuz/MetaQA); download it
and point --src at the directory containing kb.txt and the 1-hop/2-hop/3-hop
folders.

After converting, build a queryable bundle:
  cd out && cbi ingest --nodes nodes.ndjson --edges edges.ndjson --config domain.yaml \
    && cbi bundle --skill -o okf-bundle
  cbi bench eval --bundle out/okf-bundle --questions out/questions.jsonl \
    --vocab out/vocab.txt --by hop

Example:
  cbi convert metaqa --src ./MetaQA --out ./metaqa-cbi --sample 100 --hops 1,2,3`,
	RunE: runConvertMetaQA,
}

func runConvertMetaQA(cmd *cobra.Command, args []string) error {
	if err := os.MkdirAll(mqOut, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	// 1. Knowledge base → nodes/edges/vocab.
	kbPath := filepath.Join(mqSrc, "kb.txt")
	kbFile, err := os.Open(kbPath)
	if err != nil {
		return fmt.Errorf("opening kb.txt (is --src the MetaQA root?): %w", err)
	}
	graph, err := metaqa.ParseKB(kbFile)
	kbFile.Close()
	if err != nil {
		return fmt.Errorf("parsing kb.txt: %w", err)
	}
	fmt.Fprintf(os.Stderr, "kb: %d nodes, %d edges\n", len(graph.Nodes), len(graph.Edges))

	if err := writeJSONL(filepath.Join(mqOut, "nodes.ndjson"), len(graph.Nodes), func(i int) any { return graph.Nodes[i] }); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(mqOut, "edges.ndjson"), len(graph.Edges), func(i int) any { return graph.Edges[i] }); err != nil {
		return err
	}
	if err := writeLines(filepath.Join(mqOut, "vocab.txt"), graph.Vocab()); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mqOut, "domain.yaml"), []byte(metaqa.DomainYAML(mqDBName)), 0o644); err != nil {
		return fmt.Errorf("writing domain.yaml: %w", err)
	}

	// 2. QA sets → sampled questions.jsonl.
	var questions []eval.Question
	for _, hop := range mqHops {
		qaPath := filepath.Join(mqSrc, fmt.Sprintf("%d-hop", hop), "vanilla", "qa_"+mqSplit+".txt")
		f, err := os.Open(qaPath)
		if err != nil {
			return fmt.Errorf("opening %s: %w", qaPath, err)
		}
		qs, err := metaqa.ParseQuestions(f, hop)
		f.Close()
		if err != nil {
			return fmt.Errorf("parsing %s: %w", qaPath, err)
		}
		sampled := metaqa.Sample(qs, mqSample, mqSeed+int64(hop))
		fmt.Fprintf(os.Stderr, "%d-hop: %d questions → sampled %d\n", hop, len(qs), len(sampled))
		questions = append(questions, sampled...)
	}

	if err := writeJSONL(filepath.Join(mqOut, "questions.jsonl"), len(questions), func(i int) any { return questions[i] }); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nwrote %s/{nodes.ndjson,edges.ndjson,domain.yaml,vocab.txt,questions.jsonl}\n", mqOut)
	fmt.Fprintf(os.Stderr, "total: %d nodes, %d edges, %d questions\n", len(graph.Nodes), len(graph.Edges), len(questions))
	return nil
}

func writeJSONL(path string, n int, at func(i int) any) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for i := 0; i < n; i++ {
		if err := enc.Encode(at(i)); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

func writeLines(path string, lines []string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	convertMetaQACmd.Flags().StringVar(&mqSrc, "src", "", "MetaQA dataset root (contains kb.txt and N-hop/ folders) (required)")
	convertMetaQACmd.Flags().StringVar(&mqOut, "out", "metaqa-cbi", "output directory")
	convertMetaQACmd.Flags().IntSliceVar(&mqHops, "hops", []int{1, 2, 3}, "which hop sets to include")
	convertMetaQACmd.Flags().IntVar(&mqSample, "sample", 100, "questions to sample per hop (0 = all)")
	convertMetaQACmd.Flags().Int64Var(&mqSeed, "seed", 42, "random seed for question sampling")
	convertMetaQACmd.Flags().StringVar(&mqSplit, "split", "test", "QA split to draw from (train|dev|test)")
	convertMetaQACmd.Flags().StringVar(&mqDBName, "db", "metaqa.duckdb", "database_path to write into domain.yaml")
	_ = convertMetaQACmd.MarkFlagRequired("src")
	convertCmd.AddCommand(convertMetaQACmd)
	benchCmd.AddCommand(convertCmd)
}
