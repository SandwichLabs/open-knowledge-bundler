"""Deterministic: extractions/*.json + markdown/*.meta.json -> nodes.ndjson + edges.ndjson.

Emits records matching cli/domain/model.go (Node, Edge): node_id, node_type, properties,
semantic_text — without embedding or temporal fields (ingest fills those).
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path

from pipeline.canonicalize import Canonicalizer

SLUG_RE = re.compile(r"[^a-z0-9]+")


def slug(s: str) -> str:
    s = SLUG_RE.sub("-", s.lower()).strip("-")
    return s or "unknown"


# Surface form predicate -> (relationship_type, src_type, tgt_type).
# Convention: source is the broader/active party, target the entity acted upon.
PRED_MAP = {
    "WITNESSED":   ("WITNESSED_BY",  "Incident", "Person"),
    "REPORTED":    ("REPORTED_BY",   "Incident", "Person"),  # also fires when subj is Agency
    "OCCURRED_AT": ("OCCURRED_AT",   "Incident", "Location"),
    "MENTIONS":    ("MENTIONS",      "Document", None),
    "REFERENCES":  ("REFERENCES",    "Document", "Document"),
}

TYPE_PREFIX = {
    "Person":   "person",
    "Location": "location",
    "Object":   "object",
    "Agency":   "agency",
    "Date":     "date",
}


def entity_node_id(etype: str, name: str) -> str:
    return f"{TYPE_PREFIX[etype]}:{slug(name)}"


def build_semantic_text(parts: list[str]) -> str:
    return " | ".join(p for p in (p.strip() for p in parts) if p)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="in_dir", required=True,
                    help="Pipeline working dir (contains markdown/ and extractions/)")
    ap.add_argument("--out", dest="out_dir", required=True)
    ap.add_argument("--embed-endpoint", default="http://localhost:11434",
                    help="OpenAI-compatible base URL (default: ollama).")
    ap.add_argument("--embed-model", default="embeddinggemma",
                    help="Embedding model name (must be 768-dim to match ufo_domain.yaml).")
    ap.add_argument("--no-embed", action="store_true",
                    help="Skip embeddings (cbi ingest will fetch them via its own client).")
    args = ap.parse_args()

    in_dir = Path(args.in_dir)
    md_dir = in_dir / "markdown"
    ext_dir = in_dir / "extractions"
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    nodes: dict[str, dict] = {}
    edges: dict[str, dict] = {}

    def add_node(node_id: str, node_type: str, properties: dict, semantic_text: str) -> None:
        if node_id in nodes:
            existing = nodes[node_id]
            mentions = existing["properties"].setdefault("mentions", 0)
            existing["properties"]["mentions"] = mentions + properties.get("mentions", 1)
            docs = set(existing["properties"].get("source_doc_ids", []))
            docs.update(properties.get("source_doc_ids", []))
            existing["properties"]["source_doc_ids"] = sorted(docs)
            # Keep first non-empty semantic_text.
            if not existing.get("semantic_text") and semantic_text:
                existing["semantic_text"] = semantic_text
            return
        nodes[node_id] = {
            "node_id": node_id,
            "node_type": node_type,
            "properties": properties,
            "semantic_text": semantic_text,
        }

    def add_edge(rel: str, src: str, tgt: str, props: dict | None = None) -> None:
        edge_id = hashlib.sha1(f"{src}|{rel}|{tgt}".encode()).hexdigest()[:24]
        if edge_id in edges:
            return
        edges[edge_id] = {
            "edge_id": edge_id,
            "source_id": src,
            "target_id": tgt,
            "relationship_type": rel,
            "weight": 1.0,
        }
        if props:
            # Edge model doesn't expose properties, but harmless extra keys are ignored.
            edges[edge_id].update({"properties": props})

    ext_files = sorted(ext_dir.glob("*.json"))
    if not ext_files:
        print(f"No extractions found in {ext_dir}")
        return 1

    # Pass 1: feed every entity surface form to the canonicalizer.
    canon = Canonicalizer()
    for ext_path in ext_files:
        ext = json.loads(ext_path.read_text())
        for chunk in ext.get("chunks", []) or []:
            for ent in chunk.get("entities", []) or []:
                canon.feed(ent.get("type", ""), ent.get("name", ""))
    canon.build()
    stats = canon.stats()
    print("Canonicalization stats:")
    for et, s in stats["by_type"].items():
        print(f"  {et:10} raw={s['raw_surfaces']:>5}  dropped={s['dropped']:>4}  "
              f"unique_canonical={s['canonical_unique']:>5}  merge_ratio={s['merge_ratio']}")
    if stats["drop_reasons"]:
        print(f"  drop_reasons: {stats['drop_reasons']}")

    for ext_path in ext_files:
        doc_id_slug = ext_path.stem
        meta_path = md_dir / f"{doc_id_slug}.meta.json"
        if not meta_path.exists():
            print(f"[skip] no meta for {doc_id_slug}")
            continue
        meta = json.loads(meta_path.read_text())
        sha8 = meta.get("sha8", "")
        doc_node_id = f"doc:{sha8}" if sha8 else f"doc:{slug(doc_id_slug)}"

        ext = json.loads(ext_path.read_text())

        # Document node
        doc_props = {
            "title": meta.get("title", doc_id_slug),
            "filename": meta.get("filename"),
            "source_path": meta.get("source_path"),
            "sha256": meta.get("sha256"),
            "page_count": meta.get("page_count"),
            "char_count": meta.get("char_count"),
        }
        add_node(
            doc_node_id, "Document", doc_props,
            build_semantic_text([doc_props["title"], "document"]),
        )

        seen_in_doc: set[str] = set()

        for chunk in ext.get("chunks", []) or []:
            chunk_idx = chunk.get("incident", {}).get("chunk_idx", 0)
            inc = chunk.get("incident") or {}
            inc_summary = inc.get("summary", "")
            inc_date = inc.get("date_text", "")
            inc_loc = inc.get("location_text", "")
            incident_id = None
            if inc_summary or inc_date or inc_loc:
                incident_id = f"incident:{sha8}:{chunk_idx}"
                add_node(
                    incident_id, "Incident",
                    {
                        "summary": inc_summary,
                        "date_text": inc_date,
                        "location_text": inc_loc,
                        "source_doc_id": doc_node_id,
                    },
                    build_semantic_text([inc_summary, inc_date, inc_loc]),
                )
                # Document MENTIONS Incident
                add_edge("MENTIONS", doc_node_id, incident_id)

            # Entities
            name_to_id: dict[str, tuple[str, str]] = {}  # surface name -> (node_id, type)
            for ent in chunk.get("entities", []) or []:
                etype = ent["type"]
                raw_name = ent.get("name", "")
                # Canonicalize Person/Location/Object/Agency; Date passes through.
                if etype in ("Person", "Location", "Object", "Agency"):
                    name = canon.resolve(etype, raw_name)
                    if name is None:  # filtered (pronoun / stop / empty)
                        continue
                else:
                    name = raw_name
                nid = entity_node_id(etype, name)
                # Both raw and canonical map to this node, so facts resolve correctly.
                name_to_id[raw_name.lower()] = (nid, etype)
                name_to_id[name.lower()] = (nid, etype)

                attrs = ent.get("attrs") or {}
                props = {
                    "name": name,
                    "mentions": 1,
                    "source_doc_ids": [doc_node_id],
                    **{k: v for k, v in attrs.items() if v},
                }
                sem_parts = [name, etype] + [str(v) for v in attrs.values() if v]
                add_node(nid, etype, props, build_semantic_text(sem_parts))

                # Document MENTIONS entity (dedup via edge cache)
                if nid not in seen_in_doc:
                    add_edge("MENTIONS", doc_node_id, nid)
                    seen_in_doc.add(nid)

                # Incident-level edges
                if incident_id:
                    if etype == "Location":
                        add_edge("OCCURRED_AT", incident_id, nid)
                    elif etype == "Person":
                        add_edge("WITNESSED_BY", incident_id, nid)
                    elif etype == "Agency":
                        add_edge("REPORTED_BY", incident_id, nid)

            # Facts -> edges (only when both subject + object resolve to known entities)
            for fact in chunk.get("facts", []) or []:
                s = name_to_id.get(fact["subject"].lower())
                o = name_to_id.get(fact["object"].lower())
                pred = fact["predicate"]
                if not s or not o or pred not in PRED_MAP:
                    continue
                rel, _src_type, _tgt_type = PRED_MAP[pred]
                add_edge(rel, s[0], o[0], {"evidence": fact.get("evidence", "")})

    # Pre-embed via Ollama so cbi ingest skips its own client (avoids re-embedding).
    # Set --no-embed to defer this to cbi at ingest time instead.
    if not args.no_embed:
        import httpx
        from tqdm import tqdm as _tqdm

        url = f"{args.embed_endpoint.rstrip('/')}/v1/embeddings"
        items = list(nodes.values())
        print(f"Embedding {len(items)} nodes via {url} (model={args.embed_model})...")
        with httpx.Client(timeout=120.0) as client:
            for n in _tqdm(items, desc="embed"):
                text = n["semantic_text"] or n["node_id"]
                r = client.post(url, json={"model": args.embed_model, "input": text})
                r.raise_for_status()
                n["embedding"] = r.json()["data"][0]["embedding"]

    nodes_path = out_dir / "nodes.ndjson"
    edges_path = out_dir / "edges.ndjson"
    with nodes_path.open("w") as f:
        for n in nodes.values():
            f.write(json.dumps(n) + "\n")
    with edges_path.open("w") as f:
        for e in edges.values():
            f.write(json.dumps(e) + "\n")

    # Quick by-type histogram
    by_type: dict[str, int] = {}
    for n in nodes.values():
        by_type[n["node_type"]] = by_type.get(n["node_type"], 0) + 1
    rels: dict[str, int] = {}
    for e in edges.values():
        rels[e["relationship_type"]] = rels.get(e["relationship_type"], 0) + 1

    print(f"Wrote {len(nodes)} nodes -> {nodes_path}")
    print(f"  by type: {by_type}")
    print(f"Wrote {len(edges)} edges -> {edges_path}")
    print(f"  by rel:  {rels}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
