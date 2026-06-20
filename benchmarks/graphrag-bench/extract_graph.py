#!/usr/bin/env python3
"""Build a cbi knowledge graph from a text corpus via LLM entity/relation extraction.

Chunks the corpus, asks an OpenAI-compatible LLM (here the local Qwen3.6-35B
server) to extract entities + relations per chunk, de-duplicates into a node/edge
set, and emits the cbi pre-resolved ingest format (nodes.ndjson, edges.ndjson,
domain.yaml, vocab.txt). Writes incrementally so a long run can be inspected.

Usage:
  python3 extract_graph.py --corpus medical.json --out med-graph \
      --endpoint http://localhost:8080 --chunk-chars 3000 --overlap 200 [--max-chars N]
"""
import argparse, json, re, sys, time, urllib.request

NODE_TYPES = ["Disease", "Treatment", "Symptom", "Anatomy", "Drug", "Procedure",
              "Test", "RiskFactor", "Other"]

PROMPT = """Extract a medical knowledge graph from the TEXT. Return ONLY JSON, no prose, no markdown fences:
{{"entities":[{{"name":"canonical name","type":"{types}"}}],
"relations":[{{"source":"entity name","relation":"UPPER_SNAKE_VERB","target":"entity name"}}]}}
Rules: use canonical, lower-noise entity names; every relation's source and target must appear in entities; relation is an UPPER_SNAKE verb phrase (TREATED_BY, CAUSES, IS_A, DIAGNOSED_BY, RISK_FACTOR_FOR, SYMPTOM_OF, PART_OF, PREVENTS).

TEXT:
{text}"""


def chunks(text, size, overlap):
    i, n = 0, len(text)
    while i < n:
        end = min(i + size, n)
        # prefer to break on a sentence boundary near the end
        if end < n:
            m = text.rfind(". ", i + size // 2, end)
            if m != -1:
                end = m + 1
        yield text[i:end].strip()
        if end >= n:
            break
        i = max(end - overlap, i + 1)


def call_llm(endpoint, text, retries=2):
    body = json.dumps({
        "model": "x",
        "messages": [{"role": "user", "content": PROMPT.format(types="|".join(NODE_TYPES), text=text)}],
        "temperature": 0.1, "max_tokens": 2048,
        "chat_template_kwargs": {"enable_thinking": False},
    }).encode()
    for attempt in range(retries + 1):
        try:
            req = urllib.request.Request(endpoint + "/v1/chat/completions", body,
                                         {"Content-Type": "application/json"})
            r = json.load(urllib.request.urlopen(req, timeout=240))
            return r["choices"][0]["message"].get("content", "")
        except Exception as e:
            if attempt == retries:
                print(f"  ! llm error: {e}", file=sys.stderr)
                return ""
            time.sleep(2)


def parse_json(s):
    s = s.strip()
    s = re.sub(r"^```(?:json)?|```$", "", s.strip(), flags=re.MULTILINE).strip()
    a, b = s.find("{"), s.rfind("}")
    if a == -1 or b == -1:
        return None
    try:
        return json.loads(s[a:b + 1])
    except Exception:
        return None


def slug(s):
    return re.sub(r"_+", "_", re.sub(r"[^a-z0-9]+", "_", s.lower())).strip("_")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--endpoint", default="http://localhost:8080")
    ap.add_argument("--chunk-chars", type=int, default=3000)
    ap.add_argument("--overlap", type=int, default=200)
    ap.add_argument("--max-chars", type=int, default=0, help="cap corpus chars (0 = all)")
    ap.add_argument("--db", default="medical.duckdb")
    args = ap.parse_args()

    import os
    os.makedirs(args.out, exist_ok=True)
    corpus = json.load(open(args.corpus))["context"]
    if args.max_chars:
        corpus = corpus[:args.max_chars]
    all_chunks = list(chunks(corpus, args.chunk_chars, args.overlap))
    print(f"corpus {len(corpus)} chars -> {len(all_chunks)} chunks", file=sys.stderr)

    node_type = {}          # name -> type (first seen / upgraded from Other)
    node_id = {}            # name -> node_id
    edges = {}              # (src_id, rel, tgt_id) -> True
    used = set()

    def ensure(name, typ):
        name = name.strip()
        if not name:
            return None
        if name not in node_id:
            base = f"{slug(typ)[:8] or 'ent'}:{slug(name)}" or f"ent:{len(node_id)}"
            nid, k = base, 2
            while nid in used:
                nid = f"{base}_{k}"; k += 1
            used.add(nid); node_id[name] = nid; node_type[name] = typ or "Other"
        elif (node_type.get(name) in (None, "Other")) and typ and typ != "Other":
            node_type[name] = typ
        return node_id[name]

    t0 = time.time()
    for i, ch in enumerate(all_chunks):
        obj = parse_json(call_llm(args.endpoint, ch))
        ents = (obj or {}).get("entities", []) or []
        rels = (obj or {}).get("relations", []) or []
        for e in ents:
            if isinstance(e, dict):
                ensure(e.get("name", ""), e.get("type", "Other"))
        for r in rels:
            if not isinstance(r, dict):
                continue
            s = ensure(r.get("source", ""), "Other")
            t = ensure(r.get("target", ""), "Other")
            rel = slug(r.get("relation", "")).upper() or "RELATED_TO"
            if s and t and s != t:
                edges[(s, rel, t)] = True
        if (i + 1) % 5 == 0 or i + 1 == len(all_chunks):
            rate = (i + 1) / (time.time() - t0)
            print(f"  chunk {i+1}/{len(all_chunks)} | {len(node_id)} nodes {len(edges)} edges "
                  f"| {rate:.2f} chunks/s", file=sys.stderr)

    # --- write cbi ingest format ---
    id_to_name = {v: k for k, v in node_id.items()}
    with open(f"{args.out}/nodes.ndjson", "w") as f:
        for name, nid in node_id.items():
            f.write(json.dumps({
                "node_id": nid, "node_type": node_type.get(name, "Other"),
                "properties": {"name": name}, "semantic_text": name,
            }, ensure_ascii=False) + "\n")
    with open(f"{args.out}/edges.ndjson", "w") as f:
        for (s, rel, t) in edges:
            f.write(json.dumps({
                "edge_id": f"{rel.lower()}|{s}|{t}", "source_id": s, "target_id": t,
                "relationship_type": rel, "weight": 1.0,
            }, ensure_ascii=False) + "\n")
    with open(f"{args.out}/vocab.txt", "w") as f:
        for name in node_id:
            f.write(name + "\n")

    types_seen = sorted(set(node_type.values()))
    with open(f"{args.out}/domain.yaml", "w") as f:
        f.write("domain_name: medical_kg\nembedding_dim: 768\nembedding_model: gemma\n")
        f.write('endpoint_url: "http://localhost:8181"\n')
        f.write(f"database_path: {args.db}\n\nnode_definitions:\n")
        for t in types_seen:
            f.write(f"  {t}:\n    semantic_fields:\n      - name\n    mappings:\n")
            f.write('      - { source_field: "node_id", target_field: node_id, is_key: true }\n')
            f.write('      - { source_field: "name",    target_field: name }\n')
        rels = sorted({rel for (_, rel, _) in edges})
        f.write("\nedge_definitions:\n")
        for rel in rels:
            f.write(f"  {rel}:\n    mappings:\n")
            f.write('      - { source_field: "source_id", target_field: source_id, is_key: true }\n')
            f.write('      - { source_field: "target_id", target_field: target_id, is_key: true }\n')

    print(f"DONE: {len(node_id)} nodes, {len(edges)} edges, {len(types_seen)} types, "
          f"{time.time()-t0:.0f}s", file=sys.stderr)


if __name__ == "__main__":
    main()
