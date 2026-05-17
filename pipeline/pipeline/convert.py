"""Convert UFO PDFs to Markdown via Docling.

Idempotent: skips PDFs whose .md output is newer than the source.
Writes <out>/markdown/<doc_id>.md and <out>/markdown/<doc_id>.meta.json.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

from tqdm import tqdm


def doc_id_for(pdf: Path) -> tuple[str, str]:
    """Return (slug_id, sha256). slug_id = filename stem (sanitized)."""
    digest = hashlib.sha256(pdf.read_bytes()).hexdigest()
    slug = pdf.stem.replace(" ", "_")
    return slug, digest


def convert_one(pdf: Path, out_md_dir: Path, converter) -> bool:
    """Convert one PDF -> markdown + meta. Returns True if written, False if skipped."""
    slug, sha = doc_id_for(pdf)
    md_path = out_md_dir / f"{slug}.md"
    meta_path = out_md_dir / f"{slug}.meta.json"

    if md_path.exists() and meta_path.exists():
        try:
            meta = json.loads(meta_path.read_text())
            if meta.get("sha256") == sha:
                return False
        except Exception:
            pass

    result = converter.convert(str(pdf))
    md = result.document.export_to_markdown()
    md_path.write_text(md)

    meta = {
        "doc_id": slug,
        "source_path": str(pdf),
        "sha256": sha,
        "sha8": sha[:8],
        "filename": pdf.name,
        "title": pdf.stem.replace("_", " "),
        "char_count": len(md),
    }
    try:
        meta["page_count"] = len(result.document.pages) if hasattr(result.document, "pages") else None
    except Exception:
        meta["page_count"] = None

    meta_path.write_text(json.dumps(meta, indent=2))
    return True


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="in_dir", required=True, help="Directory of PDFs")
    ap.add_argument("--out", dest="out_dir", required=True, help="Output dir (will create markdown/)")
    ap.add_argument("--limit", type=int, default=0, help="Process only N PDFs (0 = all)")
    args = ap.parse_args()

    in_dir = Path(args.in_dir)
    out_md_dir = Path(args.out_dir) / "markdown"
    out_md_dir.mkdir(parents=True, exist_ok=True)

    pdfs = sorted(p for p in in_dir.rglob("*.pdf") if "__MACOSX" not in p.parts)
    if args.limit:
        pdfs = pdfs[: args.limit]
    if not pdfs:
        print(f"No PDFs found under {in_dir}", file=sys.stderr)
        return 1

    # Import here so --help works without docling installed.
    from docling.document_converter import DocumentConverter

    converter = DocumentConverter()
    written = 0
    skipped = 0
    failed = 0
    for pdf in tqdm(pdfs, desc="docling"):
        try:
            if convert_one(pdf, out_md_dir, converter):
                written += 1
            else:
                skipped += 1
        except Exception as e:
            failed += 1
            print(f"[fail] {pdf.name}: {e}", file=sys.stderr)

    print(f"Done. written={written} skipped={skipped} failed={failed} total={len(pdfs)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
