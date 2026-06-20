#!/usr/bin/env python3
"""GraphRAG-Bench questions -> cbi questions.jsonl, scoped to what the graph covers.

A question is "covered" if it mentions entities that actually exist as nodes in
the extracted graph (so a coverage failure reflects the agent, not a missing
topic). Coverage is scored by how many distinct graph entity names appear in the
question + its gold evidence; questions are then stratified-sampled by type,
preferring higher-coverage ones.
"""
import argparse, json, random, re, collections, sys

def norm(s): return " " + re.sub(r"\s+", " ", re.sub(r"[^a-z0-9 ]+", " ", (s or "").lower())).strip() + " "

ap = argparse.ArgumentParser()
ap.add_argument("--questions", default="medical_questions.json")
ap.add_argument("--graph-vocab", default="med-graph/vocab.txt", help="node names of the extracted graph")
ap.add_argument("--per-type", type=int, default=8)
ap.add_argument("--min-hits", type=int, default=1, help="min distinct graph entities a question must mention")
ap.add_argument("--seed", type=int, default=42)
ap.add_argument("--out", default="med-cbi-questions.jsonl")
args = ap.parse_args()

# "Strong" entity names: multi-word, or a single word >=6 chars (skip short
# common words like 'skin'/'cell' that would match everything).
names = []
for line in open(args.graph_vocab):
    n = line.strip()
    if not n:
        continue
    if " " in n or len(n) >= 6:
        names.append(n.lower())
names = sorted(set(names), key=len, reverse=True)
patterns = [(n, " " + re.sub(r"\s+", " ", re.sub(r"[^a-z0-9 ]+", " ", n)).strip() + " ") for n in names]

def coverage(q):
    hay = norm(q["question"]) + norm(q.get("evidence") or "") + norm(q.get("answer") or "")
    hits = {n for n, p in patterns if p in hay}
    return len(hits)

qs = json.load(open(args.questions))
scored = [(coverage(q), q) for q in qs]
covered = [(c, q) for c, q in scored if c >= args.min_hits]
print(f"{len(covered)}/{len(qs)} questions mention >= {args.min_hits} graph entities", file=sys.stderr)

by_type = collections.defaultdict(list)
for c, q in covered:
    by_type[q.get("question_type", "Unknown")].append((c, q))

rng = random.Random(args.seed)
picked = []
for t, group in by_type.items():
    # prefer higher coverage, break ties randomly for diversity
    rng.shuffle(group)
    group.sort(key=lambda x: x[0], reverse=True)
    picked.extend(q for _, q in group[:args.per_type])

with open(args.out, "w") as f:
    for q in picked:
        f.write(json.dumps({
            "id": q["id"],
            "question": q["question"],
            "gold": [q["answer"]],
            "tags": {"question_type": q.get("question_type", "Unknown")},
        }, ensure_ascii=False) + "\n")

dist = collections.Counter(q.get("question_type") for q in picked)
print(f"wrote {len(picked)} questions to {args.out}: {dict(dist)}", file=sys.stderr)
