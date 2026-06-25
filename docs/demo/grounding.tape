# grounding.tape — Demo C (hero): the local agent answers from the graph,
# and declines on what isn't there.
# Render:  vhs docs/demo/grounding.tape   (run from repo root)
# Output:  docs/demo/okb-grounding.gif

Output docs/demo/okb-grounding.gif

Set Shell bash
Set FontSize 16
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Padding 20
Set Width 1200
Set Height 760

# --- warm-up: build the bundle if needed, launch the agent, load the model ---
# (off-camera; the bundle build is instant — embeddings run in-process via kronk)
Hide
Type "cd test"
Enter
Type "test -d pokemon-bundle || (../cli/okb ingest --nodes nodes.ndjson --config domain.yaml --batch-size 50 && ../cli/okb ingest --edges edges.ndjson --config domain.yaml && ../cli/okb bundle -o pokemon-bundle/ --skill --config domain.yaml)"
Enter
Sleep 8s
Type "clear"
Enter
Sleep 500ms
Type "../cli/okb agent --bundle pokemon-bundle/"
Enter
Sleep 16s
Show
Sleep 1s

# 1) The grounded answer: the graph holds exactly three of Eevee's evolutions.
Type "List all of Eevee's evolutions."
Sleep 600ms
Enter
Sleep 11s

# 2) The honest decline: Snorlax isn't in this graph — no guessing.
Type "What is Snorlax's type?"
Sleep 600ms
Enter
Sleep 12s

Sleep 2s
Hide
Ctrl+C
