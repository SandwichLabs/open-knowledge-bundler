# bundle.tape — Demo A: from structured data to a portable knowledge bundle.
# Render:  vhs docs/demo/bundle.tape   (run from repo root)
# Output:  docs/demo/okb-bundle.gif
# Embeddings run in-process (kronk) — no embedding server needed.

Output docs/demo/okb-bundle.gif

Set Shell bash
Set FontSize 16
Set Theme "Catppuccin Mocha"
Set TypingSpeed 30ms
Set Padding 20
Set Width 1200
Set Height 760

# --- start clean in the Pokemon test fixture ---
Hide
Type "cd test && rm -rf pokemon.duckdb pokemon-bundle && clear"
Enter
Sleep 1s
Show
Sleep 500ms

# Build the graph: 36 nodes + 66 edges, embedded in-process (no server).
Type "# 1. ingest the toy graph (auto-initializes the DB, embeds locally)"
Enter
Type "../cli/okb ingest --nodes nodes.ndjson --config domain.yaml --batch-size 50"
Enter
Sleep 2s
Type "../cli/okb ingest --edges edges.ndjson --config domain.yaml"
Enter
Sleep 2s

# Pack a portable bundle: DuckDB + browsable OKF markdown + an agent Skill.
Type "# 2. pack a portable bundle: .duckdb + OKF markdown + SKILL.md"
Enter
Type "../cli/okb bundle -o pokemon-bundle/ --skill --config domain.yaml"
Enter
Sleep 2s

# The payoff: the bundle is plain, human-readable files.
Type "# 3. the bundle is just files you can read"
Enter
Type "tree pokemon-bundle/ | head -28"
Enter
Sleep 2500ms

Type "# the skill header that teaches an agent how to use it"
Enter
Type "sed -n '1,12p' pokemon-bundle/SKILL.md"
Enter
Sleep 2500ms

Type "# a concept doc — Charizard — with relationships as cross-links"
Enter
Type "sed -n '1,30p' pokemon-bundle/Pokemon/pokemon_006.md"
Enter
Sleep 5s
