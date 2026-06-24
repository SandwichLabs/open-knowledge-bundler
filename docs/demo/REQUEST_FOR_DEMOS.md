# Request for Demos — `okb` GIFs for "The Expert Is the Graph"

**Requested by:** Zac
**For:** the blog post `the-expert-is-the-graph.md` on orndorff.dev
**Tooling:** [charmbracelet/vhs](https://github.com/charmbracelet/vhs) (`.tape` → `.gif`)
**Status:** ready to produce

## TL;DR

I need **two short terminal GIFs** of `okb` in action, recorded with VHS, to drop
into specific spots in the post. They should feel like the post reads — calm,
local-first, no cloud. The shipped Pokémon test set (`test/`) is a perfect fixture:
it's tiny, it builds in seconds, and its gold answers line up exactly with the
examples in the post (especially Eevee).

Priority order: **Demo C (grounding) > Demo A (build → bundle).** If you only have
time for one, do C. (A third "agent capability" demo was considered and cut to keep
the agent from appearing twice in a dense post — see "Deferred" below.)

---

## Context the demos must respect

A few things have changed since the old `docs/demo/demo.tape` — **do not copy its
commands verbatim**:

- The binary is **`okb`**, not `cbi`.
- There is **no `okb init`** step anymore — `ingest` (and `extract`) auto-initialize
  the database. Drop the old `init` line.
- **`okb bundle`** and **`okb agent`** are new and are the stars of these demos.
- `okb ingest` needs an **OpenAI-compatible embedding endpoint** (the test
  `domain.yaml` points `endpoint_url` at `http://localhost:8080`). Have an embedding
  server up before recording the build demo (e.g. `ollama serve` with an embedding
  model, or a local llama.cpp embeddings server on `:8080`).
- `okb agent` is **fully local** (kronk/llama.cpp). First run **downloads Gemma 4 +
  EmbeddingGemma from Hugging Face** and shows an **interactive model-size picker**.
  Pre-warm before taping (see "Recording notes").

Keep the old tape's *look* (theme, dimensions, typing speed) — see styling below.

---

## Demo A — From data to a portable bundle

**What it shows:** the build pipeline producing the artifact the post calls "the
product" — a directory you can read, query, and hand to any agent.

**Post placement:** section **"The system, in one breath,"** immediately after the
line *"That bundle is the product."*

**Beats (script the tape around these):**

```bash
task build                                                   # builds cli/okb
cd test
../cli/okb ingest --nodes nodes.ndjson --config domain.yaml --batch-size 50
../cli/okb ingest --edges edges.ndjson --config domain.yaml
../cli/okb bundle -o pokemon-bundle/ --skill                 # .duckdb + OKF markdown + SKILL.md
tree pokemon-bundle/                                         # or: ls -R, or eza --tree
sed -n '1,25p' pokemon-bundle/SKILL.md                       # show the skill header
sed -n '1,30p' pokemon-bundle/Pokemon/pokemon_006.md         # Charizard concept doc w/ cross-links
```

> Verified: concept docs are named `<NodeType>/pokemon_<pokedex_id>.md` (e.g.
> `Pokemon/pokemon_006.md` is Charizard), and `bundle --skill` emits `index.md`,
> `log.md`, `catalog/`, the per-type dirs, `pokemon.duckdb`, `domain.yaml`, and
> `SKILL.md`. The toy set is 36 nodes / 66 edges and ingests in seconds.

**The payoff frame:** the `tree` output + a concept doc, so a viewer sees the bundle
is plain, human-readable markdown with relationships as clickable cross-links — not an
opaque blob. Linger on that.

**Acceptance:** ends on the bundle contents visible; total perceived runtime ≤ ~12s
after trimming; no stack traces; embedding endpoint warmed so ingest doesn't stall on
connection errors. (Confirm the exact concept-doc path/filename after the first
`bundle` run — directory casing is `<NodeType>/`.)

---

## Deferred — Agent capability (multi-hop evolution line)

Cut from this batch to avoid two agent GIFs in one post. If we want it later: TUI,
ask *"What is the full evolution line starting from Charmander?"* → Charmander →
Charmeleon → Charizard, with `hybrid_search`/`sql_query` tool-call lines visible. The
grounding demo (C) already exercises the TUI, so this is redundant for now.

---

## Demo C — Grounding: it declines instead of inventing  ★ hero demo

**What it shows:** the exact story the post tells. The graph holds **only three** of
Eevee's evolutions; a normal model would "helpfully" list all eight real ones. The
grounded agent answers from the graph — and declines on what isn't there.

**Post placement:** section **"Grounding isn't a model trait. You build it in.,"**
right after the Eevee paragraph (*"…it cheerfully returned all eight real-world
Eeveelutions."*).

**Beats (same TUI session, two questions):**

```bash
../cli/okb agent --bundle pokemon-bundle/
# 1) the grounded answer:
#    List all of Eevee's evolutions.
#       -> Vaporeon, Jolteon, Flareon   (exactly the 3 in the graph)
# 2) the honest decline (pick something genuinely absent from the toy graph):
#    What is Snorlax's type?
#       -> "I couldn't find it in the graph" (Snorlax is not a node here)
```

**Why this is the hero:** it's the post's thesis made visible — capped at three, and
an honest "not found" instead of a confident fabrication. This is gold-verified
(`test/eval-questions.jsonl` → `eevee-evos` = Vaporeon/Jolteon/Flareon).

**Acceptance:** Q1 returns exactly the three (no Espeon/Umbreon/etc.); Q2 returns an
explicit can't-find-it style decline, **not** a guess. Re-run if the model
hallucinates — the whole point is that it doesn't. (Before recording, confirm the
chosen "absent" entity really has no node; swap in another absent Pokémon if needed.)

---

## Known rendering issues (found in a trial VHS render — resolve before final capture)

I built the toy bundle and recorded Demo C end-to-end (E4B/Q4_K_M, Vulkan, Strix
Halo). The **substance is perfect**: the agent answers "Eevee evolves into Vaporeon,
Jolteon, and Flareon" via `hybrid_search` → `sql_query`, and declines on Snorlax
("I could not find … in the knowledge graph"). But three formatting problems showed up:

1. **OSC escape leaks into the echoed input line (blocker).** The `you›` line renders
   as `you› ]11;rgb:1e1e/1e1e/2e2e\List all of Eevee's evolutions.` — an OSC-11
   background-color *response* being echoed into the text input. It is a Bubble
   Tea/termenv-under-VHS interaction in `okb agent`'s TUI, **not** a tape bug. I tried
   `export COLORFGBG="15;0"` in the tape (to suppress the background query) and it did
   **not** fix it. Needs a real fix in the agent (e.g. set the lipgloss/termenv color
   profile explicitly so no background query is emitted, or the appropriate
   `tea.Without...` program option), or sidestep it (see #4 below). **This is the one
   thing that will make the GIF look broken — fix it first.**
2. **Model reasoning is dumped inline.** The TUI prints the model's natural-language
   chain-of-thought faintly before each answer ("The user is asking to list all
   evolutions of 'Eevee'. I need to use the `sql_query` tool…"). It's by design
   (`tui.go: flushReasoning`), there's no flag to hide it, and at E4B it's verbose
   enough to bury the actual answer. Options: add a "hide reasoning" toggle to the
   agent, tighten the system prompt to stop narrating, or lean in and let it show the
   work. Worth a decision before recording.
3. **Long SQL clips off the right edge** instead of wrapping (e.g. `… = SRC.node_i`).
   Widening the tape to 1400 helped only a little. Likely needs soft-wrap in the TUI's
   tool-line rendering; otherwise pick questions whose tool SQL is short.
4. **The one-shot path avoids #1 entirely.** `okb agent --ask "…"` (no TUI) printed
   clean `[tool: hybrid_search …]` / `[tool: sql_query …]` lines and the final answer
   with no leaked escapes. If the TUI bug can't be fixed quickly, recording the
   scripted one-shot form — or a `--json`-free `--ask` run — is the clean fallback. It
   loses the live-typing feel and the status bar, but it reads cleanly. **See open
   question #1.**

## Recording notes (read before you tape)

- **Pre-warm models.** Run `okb agent --bundle pokemon-bundle/` once to completion
  before recording so HF downloads and the model-size picker are done. Seed
  `~/.config/okb/config.yaml` (or run non-interactively once) so the picker does
  **not** appear mid-tape — unless you want a separate clip of the first-run picker,
  which we don't for these.
- **Latency.** Local inference is seconds per answer. Use VHS `Sleep` to wait, then
  trim dead air. Prefer editing the tape's sleeps over a global speed-up so typing
  still looks natural. Target ≤ ~8–12s perceived per GIF; loop cleanly.
- **Determinism.** Use the gold questions above. Exact phrasing of the prose may vary
  run to run — that's fine — but the **facts must match gold**. Re-record on any
  factual miss.
- **No secrets / no clutter.** `clear` first; hide the `cd`/warm-up with VHS
  `Hide`/`Show`; keep the prompt clean; no API keys on screen (there are none — that's
  on-brand, lean into it).

## Styling (match the post + existing tape)

Start from `docs/demo/demo.tape`'s look and update the commands:

```
Set Shell bash
Set FontSize 16
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Padding 20
Set Width 1200          # a touch narrower than the old 1400 → smaller files, fits blog column
Set Height 760
```

- Keep each GIF **under ~3 MB** if you can (the old `demo.gif` is 4.4 MB — fine for a
  repo, heavy for a blog). If a GIF is large, also export an `.mp4`/`webm`; we can
  decide per-asset.
- One short `# comment` line before each command is welcome (the old tape does this and
  it reads well), but keep them terse.

## Deliverables

1. Two tapes committed here: `bundle.tape`, `grounding.tape` (rename to taste).
2. The rendered GIFs.
3. **Copy the final GIFs to `orndorff.dev/static/`** named:
   - `okb-bundle.gif` (Demo A)
   - `okb-grounding.gif` (Demo C)
   They embed in the post as `/okb-bundle.gif`, etc. (site convention — same as
   `/nexus-agent-demo.gif`).
4. Suggested embed markdown for me to paste (alt text included), e.g.:

   ```markdown
   ![okb: building a portable Pokémon bundle from structured data](/okb-bundle.gif)
   ![okb: the local agent answers Eevee's evolutions from the graph — exactly three, and declines on what isn't there](/okb-grounding.gif)
   ```

## Open questions for Zac

1. **TUI vs one-shot.** I scripted the chat **TUI** (tool-call lines look great). If
   you'd rather show the one-shot `okb agent --ask "…"` form, say so — it's cleaner but
   hides the tool calls that make the grounding point land.
3. **Light or dark theme?** I defaulted to Catppuccin Mocha (matches the existing
   tape). Tell me if the blog wants a lighter terminal theme.
