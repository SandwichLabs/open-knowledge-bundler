"""Extract entities and facts from per-document markdown using a local LLM.

For each markdown file:
  1. Token-chunk the text (~1500 tokens, 200 overlap).
  2. Per chunk, POST to {endpoint}/v1/chat/completions asking for strict JSON.
  3. Persist merged result to <out>/<doc_id>.json.

Idempotent: skips docs whose extraction file already exists. Use --force to re-run.
"""
from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path

import httpx
from tqdm import tqdm


SYSTEM_PROMPT = """/no_think
You are an information extraction engine for declassified UFO/UAP documents.
Return ONLY a JSON object with this exact shape:

{
  "entities": [
    {"type": "Person|Location|Object|Agency|Date", "name": "<surface form>",
     "attrs": {"role": "...", "affiliation": "...", "value": "...", "shape": "...", "country": "..."}}
  ],
  "facts": [
    {"subject": "<entity name>", "predicate": "<one of: WITNESSED, REPORTED, OCCURRED_AT, MENTIONS, REFERENCES>",
     "object": "<entity name>", "evidence": "<short quote, <=160 chars>"}
  ],
  "incident": {
    "summary": "<<=400 chars summary of any single sighting/incident described, or empty string>",
    "date_text": "<date phrase as it appears, or empty>",
    "location_text": "<location phrase as it appears, or empty>"
  }
}

Rules:
- type MUST be one of: Person, Location, Object, Agency, Date.
- predicate MUST be one of the listed values.
- Use the literal entity surface form as the name (do not paraphrase).
- attrs values may be omitted; do not invent facts not supported by the text.
- Output ONLY the JSON object, no prose, no markdown fences."""


def chunk_text(text: str, target_tokens: int = 1500, overlap_tokens: int = 200) -> list[str]:
    """Token-chunk via tiktoken cl100k_base (good enough for any model)."""
    import tiktoken

    enc = tiktoken.get_encoding("cl100k_base")
    tokens = enc.encode(text)
    if len(tokens) <= target_tokens:
        return [text] if text.strip() else []
    chunks: list[str] = []
    step = target_tokens - overlap_tokens
    for start in range(0, len(tokens), step):
        end = min(start + target_tokens, len(tokens))
        chunks.append(enc.decode(tokens[start:end]))
        if end == len(tokens):
            break
    return chunks


def call_llm(client: httpx.Client, endpoint: str, model: str, chunk: str, retries: int = 2) -> dict | None:
    url = f"{endpoint.rstrip('/')}/v1/chat/completions"
    payload = {
        "model": model,
        "messages": [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": f"Document chunk:\n\n{chunk}"},
        ],
        "temperature": 0.0,
        "response_format": {"type": "json_object"},
        "max_tokens": 8192,
        "chat_template_kwargs": {"enable_thinking": False},
    }
    for attempt in range(retries + 1):
        try:
            r = client.post(url, json=payload, timeout=900.0)
            r.raise_for_status()
            msg = r.json()["choices"][0]["message"]
            content = msg.get("content") or msg.get("reasoning_content") or ""
            return _parse_json_loose(content)
        except (httpx.HTTPError, KeyError, ValueError) as e:
            if attempt == retries:
                preview = (content[:200] + "...") if isinstance(locals().get("content"), str) else ""
                print(f"[extract-fail] {e} preview={preview!r}", file=sys.stderr)
                return None
            time.sleep(1.5 * (attempt + 1))
    return None


def _parse_json_loose(content: str) -> dict:
    """Parse JSON tolerating ```json fences, <think> tags, or leading prose."""
    if not content:
        raise ValueError("empty content")
    s = content.strip()
    # Strip ```json ... ``` fences.
    if s.startswith("```"):
        s = s.split("\n", 1)[1] if "\n" in s else s
        if s.endswith("```"):
            s = s.rsplit("```", 1)[0]
    # Drop everything before first '{' and after last '}'.
    start = s.find("{")
    end = s.rfind("}")
    if start == -1 or end == -1 or end <= start:
        raise ValueError("no JSON object found")
    return json.loads(s[start : end + 1])


VALID_TYPES = {"Person", "Location", "Object", "Agency", "Date"}
VALID_PREDICATES = {"WITNESSED", "REPORTED", "OCCURRED_AT", "MENTIONS", "REFERENCES"}


def sanitize_extraction(raw: dict, chunk_idx: int) -> dict:
    entities = []
    for e in raw.get("entities", []) or []:
        if not isinstance(e, dict):
            continue
        t = e.get("type")
        name = (e.get("name") or "").strip()
        if t in VALID_TYPES and name:
            attrs = e.get("attrs") or {}
            if not isinstance(attrs, dict):
                attrs = {}
            entities.append({"type": t, "name": name, "attrs": attrs})

    facts = []
    for f in raw.get("facts", []) or []:
        if not isinstance(f, dict):
            continue
        s = (f.get("subject") or "").strip()
        p = f.get("predicate")
        o = (f.get("object") or "").strip()
        if s and o and p in VALID_PREDICATES:
            facts.append({"subject": s, "predicate": p, "object": o,
                          "evidence": (f.get("evidence") or "")[:160]})

    inc_raw = raw.get("incident") or {}
    incident = {
        "chunk_idx": chunk_idx,
        "summary": (inc_raw.get("summary") or "").strip()[:400],
        "date_text": (inc_raw.get("date_text") or "").strip(),
        "location_text": (inc_raw.get("location_text") or "").strip(),
    }
    return {"entities": entities, "facts": facts, "incident": incident}


def process_doc(md_path: Path, out_path: Path, client: httpx.Client, endpoint: str, model: str) -> dict:
    text = md_path.read_text()
    chunks = chunk_text(text)
    chunk_results = []
    for i, chunk in enumerate(chunks):
        raw = call_llm(client, endpoint, model, chunk)
        if raw is None:
            continue
        chunk_results.append(sanitize_extraction(raw, i))
    result = {
        "doc_id": md_path.stem,
        "chunk_count": len(chunks),
        "chunks": chunk_results,
    }
    out_path.write_text(json.dumps(result, indent=2))
    return result


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="in_dir", required=True, help="Markdown directory (from convert.py)")
    ap.add_argument("--out", dest="out_dir", required=True, help="Output directory for per-doc JSON")
    ap.add_argument("--endpoint", default="http://localhost:8080", help="OpenAI-compatible base URL")
    ap.add_argument("--model", default="gemma", help="Model name passed in chat-completions payload")
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--force", action="store_true", help="Re-run even if output exists")
    args = ap.parse_args()

    in_dir = Path(args.in_dir)
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    mds = sorted(in_dir.glob("*.md"))
    if args.limit:
        mds = mds[: args.limit]
    if not mds:
        print(f"No markdown found under {in_dir}", file=sys.stderr)
        return 1

    done = 0
    skipped = 0
    with httpx.Client() as client:
        for md in tqdm(mds, desc="extract"):
            out_path = out_dir / f"{md.stem}.json"
            if out_path.exists() and not args.force:
                skipped += 1
                continue
            try:
                process_doc(md, out_path, client, args.endpoint, args.model)
                done += 1
            except Exception as e:
                print(f"[fail] {md.name}: {e}", file=sys.stderr)

    print(f"Done. extracted={done} skipped={skipped} total={len(mds)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
