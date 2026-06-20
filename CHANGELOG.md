# Changelog

All notable changes to `cbi` (the graph-search-tool CLI) are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased] — 2026-06-19

### Added — `cbi answer` + tool-result capture (external-benchmark prep)

- **`cbi answer --bundle <dir> --questions q.jsonl`** batch-answers a question set
  with the local agent (model loads once, in-process) and emits, per question, the
  `generated_answer` plus the **retrieved `context`** (concatenated tool outputs)
  and run metadata, as a JSON array. Unlike `cbi eval` it does not score — it
  produces raw answers + context for an external judge (e.g. GraphRAG-Bench).
- **Tool-result capture:** `--ask --json` (via `Session.Answer`) now records
  `tool_results` (each tool's output text), not just the tool calls. The streaming
  `StreamHandler.OnToolResult` callback gained an `output` argument. This is what
  feeds the `context` field external retrieval/faithfulness metrics need.

### Added — `cbi eval`: benchmark the agent against a known-answer set

`cbi eval --bundle <dir> --questions q.jsonl` runs the local agent over a
`questions.jsonl` answer key and scores its answers **deterministically** (no LLM
judge). The model loads once per tier and answers every question in-process — far
faster than spawning `cbi agent --ask` per question.

- **Metrics** (new `cli/eval` package): `recall` (gold coverage), `exact`
  (Hits@all — every gold item present), and, when an entity-name `--vocab` is
  available, `precision`/`F1`. Precision counts known entities the answer named
  that are *not* in the gold set — which catches over-generation (the
  hallucination mode) automatically, no judge needed. Entities named in the
  question itself (the topic/subject) are excluded so restating them isn't
  miscounted as over-generation.
- **Honest miss vs confident wrong** are tallied separately: an answer that
  disclaims ("not found in the graph") is a categorically better failure than a
  fabricated one, so the leaderboard tracks them apart.
- **Sweep + breakdown:** pass `--tier` more than once to compare model sizes;
  `--by <tag>` breaks the leaderboard down by a question tag (e.g. `hop`).
  `--out` writes per-question results (answer + tool-call trace + score) as JSONL.
- Built on the `--ask --json` primitive: reuses the agent session's new
  in-process `Answer()` method and `ResetHistory()` between independent questions.

### Added — `cbi convert metaqa`: MetaQA → cbi domain + eval set

`cbi convert metaqa --src <MetaQA dir> --out <dir>` converts a local copy of
[MetaQA](https://github.com/yuyuz/MetaQA) (the WikiMovies knowledge base + the
1/2/3-hop QA sets) into a ready-to-ingest bundle plus a matching answer key:
`nodes.ndjson`, `edges.ndjson`, `domain.yaml`, `vocab.txt` (for `cbi eval
--vocab`), and a sampled `questions.jsonl` tagged by hop. Node types
(Movie/Person/Year/Genre/…) are inferred from the relation (every kb triple has a
movie subject); `--sample N` caps questions per hop, `--hops`/`--split`/`--seed`
control selection. New `cli/metaqa` package holds the parsing/typing logic.

### Added — `cbi agent`: self-contained local LLM chat over OKF bundles

A new `cbi agent --bundle <dir>` command opens an interactive chat TUI (or a
one-shot `--ask` answer) that reasons over an OKF bundle **entirely on-device** —
no API keys, no cloud, no embedding server. Inference and embeddings run locally
via [kronk](https://github.com/ardanlabs/kronk) (llama.cpp); the agent loop, tool
calling, and token streaming are handled by
[fantasy](https://github.com/charmbracelet/fantasy).

New `cli/agent/` package:

- **`bundle.go`** — loads a bundle: finds the domain config (any `*_domain.yaml`
  name, not just `domain.yaml`), resolves the bundled `.duckdb`, and indexes the
  markdown concept docs. `ReadDoc` is path-confined to the bundle directory.
- **`config.go`** — persistent user config at `~/.config/cbi/config.yaml` via a
  dedicated viper instance. First run prompts a model-size picker; Gemma 4 tier
  presets (`small`..`xl`/`moe`), embedding source, and llama.cpp backend persist.
- **`embed.go`** — local embeddings via the kronk SDK (`krn.Embeddings`),
  Matryoshka-reduced to the bundle's index dimension (EmbeddingGemma, 768-dim).
- **`tools.go`** — the tools the agent calls: `schema`, `sql_query` (read-only
  guard), `hybrid_search` (vector + lexical, with a lexical-only fallback),
  `list_docs`, `search_docs`, `read_doc`.
- **`runner.go`** — assembles the fantasy agent (system prompt + tools), bridges
  streaming events, and threads multi-turn history.
- **`tui.go`** — Bubble Tea chat UI (scrollback viewport with glamour-rendered
  markdown, tool-call lines, status bar).
- **`session.go`** — orchestrates DB + models + tools; `Run` (TUI) and `RunOnce`
  (`--ask`).
- **`cli/cmd/agent.go`** — the cobra command. Flags: `--bundle`, `--db`, `--tier`,
  `--model`, `--gpu`, `--reconfigure`, `--ask`.

Behavior notes:

- **Vulkan by default.** kronk's auto-detect prefers ROCm when `rocminfo` is
  present, which is unreliable on AMD APUs (e.g. Strix Halo). The agent defaults
  `processor: vulkan` and exports `KRONK_PROCESSOR` before any kronk call so the
  embedded provider's library loader uses it too. Override with `--gpu` / config.
- **Model defaults** are the un-gated **unsloth Gemma 4 GGUF** mirrors
  (`unsloth/gemma-4-*-it-GGUF:Q4_K_M`); the ggml-org mirrors only ship `Q8_0`/`bf16`,
  so a `:Q4_K_M` tag selector fails there. Embeddings default to
  `unsloth/embeddinggemma-300m-GGUF:Q8_0`. Models download from Hugging Face once.
- **Graceful degradation.** If the embedding model can't load or its dimension
  doesn't match the bundle's index, `hybrid_search` drops to lexical-only and the
  TUI status bar says so.

### Added — `cbi agent --ask --json` (machine-readable one-shot)

`--ask` paired with `--json` emits a single JSON object on stdout instead of
streamed prose, so an eval harness can grade without scraping text. The object
carries the final `answer`, the `tool_calls` trace (name + raw input, in call
order — lets you measure tool selection and SQL authoring), `steps`, token
`usage`, `duration_ms`, `vector_ok`, and any `warning`/`error`. kronk's own
download/load logging (which writes to stdout) is redirected to stderr in this
mode so stdout stays pure JSON. This is the enabling primitive for the planned
benchmark driver (sweep model tier × domain, score answers against a key).

Caveat: `usage.output_tokens` is whatever the kronk provider reports per step,
which under-counts generated tokens today; `duration_ms` is the reliable local
cost signal.

### Added — store helpers

- `store.RawQueryArgs(query, args...)` and exported `store.ScanSearchRows` to
  support the agent's parameterized lexical-search fallback.

### Fixed / Changed — agent answer accuracy (grounding pass)

Found by grading the agent against a known-answer Pokémon graph; recall went
**3/6 → 5/6**, with the sixth now failing honestly instead of confabulating. See
the write-up: *Grounding a Fully-Local GraphRAG Agent* on orndorff.dev.

- **JSON operator-precedence bug.** `properties->>'key'='value'` (unparenthesized)
  makes DuckDB bind `->>` to the boolean and raises `Conversion Error: Failed to
  cast value to numerical`. The `schema` tool's worked example used the broken
  form, teaching the model to repeat it. Now everything uses
  `(properties->>'key') = 'value'`, with an explicit rule in the prompt and schema.
- **Schema now exposes property keys per node type** (via `json_keys`) and edge
  direction, so the model stops guessing field names (e.g. `trainer_name` vs
  `name`) and stops inverting relationships.
- **Anti-hallucination guardrail.** A hard system-prompt rule: answer only from
  tool results in the current conversation; say "not found in the graph" rather
  than completing or correcting lists from background knowledge. (Fixes the model
  returning all eight real-world Eevee evolutions when the graph held three.)
- **Query guidance.** Prefer plain `Edges_Base`/`Nodes_Base` joins over duckpgq
  PGQ for traversals; no recursive CTEs or inline `{property: value}` match filters.
- **Tool-step budget** raised from 12 → 20 so a bad query can be recovered.
- Optional tool inputs (`date`, `limit`, `prefix`) marked `omitempty` so fantasy
  does not advertise them as required parameters.

### Dependencies

- Added `charm.land/fantasy` (+ `providers/kronk`), `github.com/ardanlabs/kronk`,
  and `github.com/charmbracelet/{bubbletea,bubbles,glamour}` + `lipgloss`.

## Earlier

Prior capabilities already in the CLI (summarized; see git history for detail):

- **OKF export** — `cbi generate okf` writes an Open Knowledge Format v0.1 bundle
  (markdown concepts with YAML frontmatter); `--skill` adds a `SKILL.md` and
  `--include-db` copies the DuckDB database + domain config for a self-contained
  agent skill.
- **Static site bundle** — `cbi generate` emits a self-contained, cache-busted
  static bundle (with a D3 graph viewer) for S3/object-storage hosting.
- **Hybrid search** — `cbi query` runs BM25 (`fts`) + vector (`vss`) retrieval
  fused with Reciprocal Rank Fusion, with optional temporal filtering.
- **Graph queries** — `cbi graph` runs raw SQL / SQL-PGQ (`duckpgq`) over the
  `domain_graph` property graph.
- **Temporal model** — SCD Type 2 versioning on nodes and edges (`valid_from`,
  `valid_to`, `is_current`).
