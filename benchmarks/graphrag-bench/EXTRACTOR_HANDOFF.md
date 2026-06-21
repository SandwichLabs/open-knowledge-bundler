# `cbi extract` — Fully-Local Graph Extraction Pipeline (Build Handoff)

**Status:** designed, not yet built. This document is a cold-start spec: a fresh
agent should be able to implement the whole feature from it without re-deriving
the integration points.

**Motivation:** the throwaway `extract_graph.py` (Qwen-35B over HTTP, single
pass) built the GraphRAG-Bench medical graph, and a benchmarking exercise
(local Gemma agent + Sonnet subagents over the OKF bundle, scored by a local
Qwen judge) exposed concrete, repeatable weakpoints in *the graph*, not the
answerer. This pipeline addresses them and folds extraction into `cbi` itself,
in-process, no external LLM server.

---

## 1. Weakpoints to fix (evidence from the benchmark)

| # | Weakpoint | Evidence |
|---|---|---|
| 1 | **No entity resolution** → duplicate nodes | `disease:hodgkin_lymphoma` / `_2` / `_3`; `adrenocortical_carcinoma` vs `adrenocortical_cancer`; `nasopharyngeal_carcinoma` vs `nasopharyngeal_cancer`. Facts scatter across duplicates; diagnostics landed unevenly on `hodgkin_lymphoma_2`. |
| 2 | **Uncontrolled relation vocabulary** | ~150 relationship types with synonym sprawl (`TREATS`/`TREATED_BY`/`USED_TO_TREAT`; `DIAGNOSED_BY`/`USED_TO_DIAGNOSE`/`DETECTS`/`TESTS_FOR`) and inconsistent direction (`HAS_SYMPTOM` vs `SYMPTOM_OF`). Forces every query to check both edge directions. |
| 3 | **Low recall / missing nodes & edges** | `adrenocortical_carcinoma` had only `IS_A` + `ORIGINATES_IN`, no symptom edges (gold lists ~15 symptoms); `pheochromocytoma` had **no node at all**. Single pass, `max_tokens=2048`, chunk boundaries split facts. |
| 4 | **Wrong granularity** | Symptoms/diagnostics attached to an `IS_A` parent (`adrenal_tumors`) instead of the specific disease; child nodes inherited nothing. |
| 5 | **Brittle output parsing** | Regex-scrape of the first `{`…`}`; malformed JSON silently dropped a whole chunk. |
| 6 | **Not self-contained** | Requires a running llama-server (Qwen) on `:8080`; lives in `benchmarks/`, not shipped in `cbi`. |

`extract_graph.py` (this dir) hits every one of these — read it as the baseline.

---

## 2. Decisions (confirmed with user)

- **Ontology = bootstrap, then editable.** An LLM pass proposes the entity-type +
  closed-relation vocabulary (with canonical direction) from a corpus sample,
  writes it into `domain.yaml`, and the user can edit/approve before the full
  run. Keeps `cbi` domain-agnostic — **no hardcoded medical schema**.
- **Scope = full five-stage pipeline in one go** (extract → glean → resolve →
  normalize → emit/ingest), benchmarked as a whole.
- **Inference = in-process, 12–32B**, via the kronk SDK (`Chat`), default tier
  `large` (Gemma-4-12B), `xl`/`moe`/Qwen selectable. No HTTP server.

---

## 3. Architecture

New package `cli/extract` + command `cli/cmd/extract.go`. Reuses the agent's
kronk plumbing and config.

```
cli/extract/ontology.go    Ontology type; bootstrap (LLM proposes from sample);
                           load/save into domain.yaml's `ontology:` block.
cli/extract/llm.go         Generator: wraps *kk.Kronk (generation model); 
                           Generate(ctx, system, user, schema) -> string.
cli/extract/chunk.go       Sentence-aware chunker (size + overlap).
cli/extract/extract.go     Stage 1–2 driver: per-chunk extract + gleaning.
cli/extract/resolve.go     Stage 3: entity resolution (normalize + embed-cluster
                           + LLM-adjudicate -> canonical nodes, remap edges).
cli/extract/normalize.go   Stage 4: relation -> canonical form + direction.
cli/extract/emit.go        Stage 5: write nodes/edges NDJSON + domain.yaml + vocab.
cli/cmd/extract.go         cobra command; flags; model load; run; optional --ingest.
```

### Command surface

```
cbi extract --corpus <file|dir> --config domain.yaml -o out/ \
    [--bootstrap]            # (re)derive ontology from a corpus sample, write to domain.yaml, then run
    [--chunk-chars 6000] [--overlap 400] \
    [--glean 1]              # extra recall passes per chunk (0 = off)
    [--resolve] [--resolve-threshold 0.86] \
    [--tier large|xl|moe] [--model <src>] [--gpu vulkan] \
    [--ingest]               # chain init+ingest+index in the same run (fully local)
```

`--corpus` accepts the GraphRAG-Bench `medical.json` shape (`{"context": "..."}`)
and plain `.txt`/dir-of-text. Detect by extension/JSON-probe.

---

## 4. Stage detail

### Stage 0 — Ontology (`ontology.go`)
- Add an `Ontology` block to `domain.DomainConfig` (see §6) holding:
  - `entity_types: [{name, description}]`
  - `relations: [{name, source_type, target_type, description}]` — **name is the
    canonical relation, direction is `source_type -> target_type`.**
- **Bootstrap** (`--bootstrap` or when the block is absent): sample N chunks
  spread across the corpus, ask the model (grammar-constrained, see §5) to
  propose a *compact* ontology (target ≤ ~20 entity types, ≤ ~40 relations,
  no synonyms — instruct it to merge `TREATS`/`TREATED_BY` into one directional
  relation). Write into `domain.yaml`, print a notice to review, and (unless
  `--yes`) stop so the user can edit, OR continue if `--bootstrap` was explicit.
- The ontology becomes a **closed enum** used in every later prompt and grammar.

### Stage 1 — Extract (`extract.go`, grammar-constrained)
- Chunk corpus (sentence-aware; default 6000 chars / 400 overlap — we now load
  128k context so chunks can be large).
- Per chunk, `Generator.Generate` with a JSON schema forcing:
  ```json
  {"entities":[{"name":"...","type":"<enum>","aliases":["..."]}],
   "relations":[{"source":"...","relation":"<enum>","target":"..."}]}
  ```
  `type` and `relation` are `enum`-constrained to the ontology (GBNF restricts
  sampling — this is how #2 dies at the token level and #5 disappears entirely).
  Raise `max_tokens` to 4096–8192.
- Accumulate raw mentions: `name -> {type, aliases, chunkIDs}` and
  `(source,relation,target,chunkID)`.

### Stage 2 — Gleaning (`extract.go`, recall — fixes #3)
- For `--glean K` rounds, re-prompt each chunk with the entities already
  extracted from it and ask for **only what was missed** (same schema). Stop
  early when a round adds nothing. Standard GraphRAG gleaning; materially lifts
  recall on dense passages.

### Stage 3 — Entity resolution (`resolve.go`, fixes #1, helps #4)
1. **Exact/near-exact merge:** normalize names (lowercase, strip punctuation,
   singularize trivially) → merge identical keys.
2. **Embedding cluster:** embed each surviving candidate with the **in-process
   `agent.Embedder`** (reuse — same EmbeddingGemma 768-dim used by bundles).
   Within each entity *type*, cluster by cosine ≥ `--resolve-threshold`.
3. **LLM adjudication** for borderline pairs/clusters: grammar-constrained
   `{"same": true|false, "canonical": "..."}`. Only call the LLM for pairs in a
   gray band (e.g. cosine 0.80–0.92) to bound cost.
4. **Canonicalize:** pick one node per cluster (shortest non-abbreviated name or
   highest-degree), union the rest as `properties.aliases` (improves future
   lexical/hybrid recall), and **remap every edge endpoint** to the canonical id.
   Collapsing duplicates re-attaches scattered facts to one node (helps #4).

### Stage 4 — Relation normalization + direction (`normalize.go`, fixes #2)
- For each `(source, relation, target)`: look up `relation` in the ontology.
  - If it matches a canonical relation, keep.
  - If it's a known **inverse** (maintain an inverse map, e.g. `SYMPTOM_OF` ↔
    `HAS_SYMPTOM`), rewrite to canonical and **swap source/target**.
  - If off-vocabulary (should be rare given the enum grammar, but gleaning/bootstrap
    drift happens), LLM-map to the nearest canonical relation or drop to an
    `OTHER` bucket — and **log the count** (no silent loss).
- Validate endpoint types against the relation's declared `source_type ->
  target_type`; flip or drop type-mismatched edges. Dedup after remap.

### Stage 5 — Emit / ingest (`emit.go`)
- Write `nodes.ndjson`, `edges.ndjson`, `domain.yaml`, `vocab.txt` in the
  existing cbi ingest shape (match `extract_graph.py` output exactly so the rest
  of the toolchain is unchanged). Carry `properties.aliases` and
  `properties.provenance` (source chunk ids) for traceability.
- `--ingest`: chain `store.Open` → `LoadExtensions` → `UpsertNodes/UpsertEdges`
  → `RebuildFTSIndex` in-process. **Embeddings:** use the in-process
  `agent.Embedder` (NOT the HTTP `embed.Client`) so the whole command is
  serverless. (Today `cmd/ingest.go` uses `embed.NewClient` over HTTP — either
  factor its core into a function that accepts an `Embed(ctx,text)([]float32)`
  interface and pass the in-process embedder, or replicate the upsert loop.)

---

## 5. kronk integration (VERIFIED against v1.28.0 — use these exact shapes)

**In-process generation** mirrors `cli/agent/embed.go`'s setup. Load llama.cpp +
model, then call `Chat`. Two kronk instances (this generator + `agent.Embedder`)
co-reside fine — the agent already runs an LLM + embedder together.

```go
import (
    kk "github.com/ardanlabs/kronk/sdk/kronk"
    "github.com/ardanlabs/kronk/sdk/kronk/applog"
    "github.com/ardanlabs/kronk/sdk/kronk/model"
    "github.com/ardanlabs/kronk/sdk/tools/libs"
    "github.com/ardanlabs/kronk/sdk/tools/models"
)

// setup (once): libs.New().Download(ctx, log); models.New().Download(ctx, log, source)
// -> mp; kk.Init(); krn, _ := kk.New(model.WithModelFiles(mp.ModelFiles), model.WithAutoTune(true))
// processor backend is read from KRONK_PROCESSOR env (set it before kk calls, as cmd/agent.go does).

func (g *Generator) Generate(ctx context.Context, system, user string, schema model.D) (string, error) {
    if _, ok := ctx.Deadline(); !ok { // kronk REQUIRES a deadline
        var cancel context.CancelFunc
        ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
        defer cancel()
    }
    d := model.D{
        "messages": []model.D{
            {"role": "system", "content": system},
            {"role": "user", "content": user},
        },
        "temperature":     0.1,
        "max_tokens":      8192,
        "enable_thinking": false, // Gemma/Qwen: skip the think phase for extraction
    }
    if schema != nil {
        // response_format -> compiled to a GBNF grammar internally (grammar.go).
        // Accepts {"type":"json_schema","json_schema":{"schema": <jsonschema map>}}.
        d["response_format"] = model.D{
            "type":        "json_schema",
            "json_schema": model.D{"schema": schema},
        }
    }
    resp, err := g.krn.Chat(ctx, d)
    if err != nil { return "", err }
    if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
        return "", fmt.Errorf("empty chat response")
    }
    return resp.Choices[0].Message.Content, nil // grammar guarantees valid JSON
}
```

Key facts:
- `model.D` is `map[string]any`. Messages are `[]model.D` of `{"role","content"}`.
- `Chat(ctx, d)` returns `model.ChatResponse`; text is
  `resp.Choices[0].Message.Content` (`Message` is `*ResponseMessage`, may be nil
  on error). `resp.Usage` (`*model.Usage`) has token counts + `TokensPerSecond`.
- Recognized `model.D` keys include: `messages`, `temperature`, `max_tokens`,
  `top_p`, `top_k`, `min_p`, `repeat_penalty`, `enable_thinking`,
  `reasoning_effort`, `grammar`, `json_schema`, `response_format`, `tools`,
  `stream`, `include_usage`. (Full list grepped from `sdk/kronk/model/*.go`.)
- **Structured output:** prefer `response_format` (OpenAI-shaped, → grammar) over
  raw `grammar`. `fromResponseFormat` (grammar.go:83) accepts the schema under
  `json_schema.schema` or directly under `json_schema`.
- `Chat` errors if the context has **no deadline** — always attach one.
- Embeddings: reuse `agent.NewEmbedder(ctx, source, dim, log)` →
  `Embedder.Embed(ctx, text) ([]float32, error)` (already in `cli/agent/embed.go`).

---

## 6. Data types & reuse points

- **`domain.DomainConfig`** (`cli/domain/config.go`): `DomainName`, `EmbeddingDim`,
  `EmbeddingModel`, `EndpointURL`, `DatabasePath`, `NodeDefinitions`,
  `EdgeDefinitions map[string]EntityDef`. **Add** `Ontology *Ontology
  \`yaml:"ontology,omitempty"\`` (new type below). The bootstrap writes it; the
  emit step also regenerates `NodeDefinitions`/`EdgeDefinitions` from the
  resolved types (as `extract_graph.py` does).
- **`domain.Node` / `domain.Edge`** (`cli/domain`, used in `cmd/ingest.go`):
  Node = `{NodeID, NodeType, Properties map, SemanticText, Embedding []float32,
  ValidFrom, IsCurrent}`; Edge = `{EdgeID, SourceID, TargetID, RelationshipType,
  Weight, ValidFrom, IsCurrent}`. NDJSON field names: `node_id, node_type,
  properties, semantic_text` and `edge_id, source_id, target_id,
  relationship_type, weight`.
- **`agent.Config`** (`cli/agent/config.go`): tiers (`small`..`moe`),
  `LLMSource()`, `EmbedSource`, `Processor`. Reuse for `--tier/--model/--gpu`
  and to set `KRONK_PROCESSOR`. Consider adding an `extract_tier` default = `large`.
- **`store.DB`** (`cli/store`): `Open`, `LoadExtensions`, `UpsertNodes`,
  `UpsertEdges`, `RebuildFTSIndex` — for `--ingest`.

New ontology type (put in `cli/domain` so config can embed it):
```go
type RelationDef struct {
    Name       string `yaml:"name"`
    SourceType string `yaml:"source_type"`
    TargetType string `yaml:"target_type"`
    Inverse    string `yaml:"inverse,omitempty"`   // canonical inverse name, if any
    Description string `yaml:"description,omitempty"`
}
type TypeDef struct {
    Name        string `yaml:"name"`
    Description string `yaml:"description,omitempty"`
}
type Ontology struct {
    EntityTypes []TypeDef     `yaml:"entity_types"`
    Relations   []RelationDef `yaml:"relations"`
}
```

---

## 7. Validation = today's benchmark as a regression test

The exercise that found these weakpoints is the acceptance test. After building:

1. Re-extract the GraphRAG-Bench medical corpus:
   `cbi extract --corpus /tmp/grbench/medical.json --config med-domain.yaml -o med-graph-v2/ --bootstrap --glean 1 --resolve --ingest`
2. Structural deltas to report:
   - relation-vocab size: ~150 → target ≤ ~40
   - duplicate-cluster count: → ~0 (verify hodgkin = one node)
   - node/edge counts; alias coverage
   - spot-check known failures: `adrenocortical_carcinoma` now has direct
     symptom edges; `pheochromocytoma` node exists.
3. Answer-quality delta (the real number): re-bundle (`cbi generate okf
   --skill --include-db`), re-run **the same 32 questions** through both paths:
   - local agent: `cbi answer --bundle … --questions /tmp/grbench/med-q.jsonl`
   - Sonnet subagents over the bundle (see the harness used in this session;
     answers → `to_grbench.py` → `run_judge.sh`)
   Compare `answer_correctness` by question type vs the v1 baselines:
   - local Gemma-12B@32 steps: **0.520 overall** (Fact 0.404 / Complex 0.590 /
     Summarize 0.565 / Creative 0.520)
   - Sonnet-over-bundle: **see `med-judge-sonnet.json`** (judging in progress).
   Fact Retrieval is the weakpoint most bottlenecked by extraction, so it's the
   headline metric to move.

Baseline artifacts live in `/tmp/grbench/`: `med-q.jsonl` (32-Q answer key),
`med-answers-*.json` (answers per config), `med-judge-*.json` (judge scores),
`grbench_results_*.json` (judge input), `to_grbench.py`, `run_judge.sh`,
`GraphRAG-Benchmark/` (the patched eval: `generation_eval.py` has
`enable_thinking=false` + `timeout=300`). Servers: Qwen judge `:8080`,
embeddinggemma `:8181`.

---

## 8. Risks / open notes

- **Two models co-resident.** Generation (12–32B) + embedder (300M) load
  together — fine on Strix Halo (128 GB unified). The earlier `vk::DeviceLostError`
  was **12B@128k co-resident with the 37 GB Qwen judge**, not 12B + the tiny
  embedder. Keep extraction chunk context modest; you don't need 128k for
  per-chunk extraction.
- **Bootstrap quality gates everything.** A sloppy ontology re-introduces #2.
  Make the bootstrap prompt explicit about merging synonyms and declaring one
  direction per relation; let the user edit `domain.yaml` before the full run
  (the chosen "bootstrap, then editable" flow).
- **Resolution cost.** All-pairs LLM adjudication is O(n²). Gate it: cluster by
  embedding first, only adjudicate gray-band pairs, cap per-type.
- **In-process embeddings for `--ingest`.** `cmd/ingest.go` currently embeds over
  HTTP. To be fully serverless, refactor its embed step behind an interface and
  pass `agent.Embedder`. Until then, `--ingest` still needs `:8181`, but pure
  `cbi extract` (emit only) is already fully local.
- **Speed.** v1 full extraction was ~10,188 s (~2.8 h) for 386 chunks on Qwen-35B
  over HTTP. In-process 12B with grammar constraints should be faster per token
  and avoids HTTP overhead; gleaning adds passes. Report wall-clock and
  tokens/sec from `resp.Usage`.

---

## 9. Build checklist

- [ ] `cli/domain/config.go`: add `Ontology`, `RelationDef`, `TypeDef`.
- [ ] `cli/extract/llm.go`: `Generator` (load + `Generate` with `response_format`).
- [ ] `cli/extract/chunk.go`: sentence-aware chunker.
- [ ] `cli/extract/ontology.go`: bootstrap + load/save to `domain.yaml`.
- [ ] `cli/extract/extract.go`: stage 1–2 (extract + glean) accumulation.
- [ ] `cli/extract/resolve.go`: stage 3 (normalize + embed-cluster + adjudicate + remap).
- [ ] `cli/extract/normalize.go`: stage 4 (relation canonicalize + direction).
- [ ] `cli/extract/emit.go`: stage 5 (NDJSON + domain.yaml + vocab; optional ingest).
- [ ] `cli/cmd/extract.go`: flags, config/processor, model load, run, `--ingest`.
- [ ] `go build ./... && go vet ./...` (cgo links kronk + duckdb).
- [ ] Validate against the 32-Q benchmark (§7); update `CHANGELOG.md` + a blog post.
</content>
</invoke>
