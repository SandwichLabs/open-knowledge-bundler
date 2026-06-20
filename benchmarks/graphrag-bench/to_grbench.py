#!/usr/bin/env python3
"""cbi answer output -> GraphRAG-Bench results JSON.

GraphRAG-Bench's generation_eval expects records with:
  id, question, generated_answer, ground_truth, context, question_type
We carry ground_truth in gold[0] and question_type in tags, so this is a remap.
"""
import argparse, json, sys

ap = argparse.ArgumentParser()
ap.add_argument("--answers", required=True, help="cbi answer JSON array")
ap.add_argument("--out", default="grbench_results.json")
args = ap.parse_args()

recs = json.load(open(args.answers))
out = []
for r in recs:
    out.append({
        "id": r["id"],
        "question": r["question"],
        "generated_answer": r.get("generated_answer", ""),
        "ground_truth": (r.get("gold") or [""])[0],
        "context": r.get("context", ""),
        "question_type": (r.get("tags") or {}).get("question_type", "Unknown"),
    })

json.dump(out, open(args.out, "w"), ensure_ascii=False, indent=2)
print(f"wrote {len(out)} GraphRAG-Bench records to {args.out}", file=sys.stderr)
