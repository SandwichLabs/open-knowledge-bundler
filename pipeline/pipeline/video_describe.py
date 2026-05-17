"""Extract N frames per MP4 and ask a multimodal LLM to describe each.

For every <video>.mp4 under --in, writes:
  <out>/frames/<video_id>/frame_NN.jpg      (sampled frames)
  <out>/markdown/<video_id>.md              (per-frame descriptions, ready for extract.py)
  <out>/markdown/<video_id>.meta.json       (matches docling output schema so build_ndjson works unchanged)

Idempotent: skips videos with an existing .md whose meta.sha256 matches the source mp4.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import shutil
import subprocess
import sys
from pathlib import Path

import httpx
from tqdm import tqdm


FRAME_PROMPT = """/no_think
You are analyzing a frame from a declassified U.S. government UAP/UFO video.
Describe what is visible in 3-4 sentences. Be concrete:
- Objects/shapes (orbs, craft, lights, aircraft)
- Camera/sensor type (thermal/IR, optical, gun-camera)
- HUD/overlay text or markings (coordinates, dates, instrument readouts)
- Motion cues if any (blur, trail, position-change context)
Do not invent dates or locations beyond what is on the HUD.
Plain prose only — no bullets, no JSON."""


def video_sha(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def probe_duration(path: Path) -> float:
    out = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "v:0",
         "-show_entries", "stream=duration", "-of", "default=nw=1:nk=1", str(path)],
        capture_output=True, text=True, check=True,
    ).stdout.strip()
    try:
        return float(out)
    except ValueError:
        return 0.0


def extract_frames(video: Path, dest_dir: Path, n: int, width: int) -> list[Path]:
    """Sample n evenly-spaced frames. Returns list of frame paths."""
    dest_dir.mkdir(parents=True, exist_ok=True)
    for old in dest_dir.glob("frame_*.jpg"):
        old.unlink()

    dur = probe_duration(video)
    if dur <= 0:
        return []
    # Pick timestamps at the centers of n equal segments.
    timestamps = [(i + 0.5) * dur / n for i in range(n)]
    out_paths: list[Path] = []
    for i, ts in enumerate(timestamps):
        out = dest_dir / f"frame_{i:02d}.jpg"
        subprocess.run(
            ["ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
             "-ss", f"{ts:.3f}", "-i", str(video),
             "-frames:v", "1", "-vf", f"scale={width}:-1",
             "-q:v", "4", str(out)],
            check=True,
        )
        if out.exists() and out.stat().st_size > 0:
            out_paths.append(out)
    return out_paths


def describe_frame(client: httpx.Client, endpoint: str, model: str, frame: Path,
                   retries: int = 2) -> str:
    url = f"{endpoint.rstrip('/')}/v1/chat/completions"
    b64 = base64.b64encode(frame.read_bytes()).decode()
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": FRAME_PROMPT},
            {"type": "image_url", "image_url": {"url": f"data:image/jpeg;base64,{b64}"}},
        ]}],
        "temperature": 0.1,
        "max_tokens": 400,
        "chat_template_kwargs": {"enable_thinking": False},
    }
    for attempt in range(retries + 1):
        try:
            r = client.post(url, json=payload, timeout=600.0)
            r.raise_for_status()
            msg = r.json()["choices"][0]["message"]
            return (msg.get("content") or msg.get("reasoning_content") or "").strip()
        except (httpx.HTTPError, KeyError, ValueError) as e:
            if attempt == retries:
                print(f"[describe-fail] {frame.name}: {e}", file=sys.stderr)
                return ""
    return ""


def process_video(video: Path, out_root: Path, client: httpx.Client, endpoint: str,
                  model: str, n_frames: int, width: int) -> bool:
    slug = video.stem.replace(" ", "_")
    md_path = out_root / "markdown" / f"{slug}.md"
    meta_path = out_root / "markdown" / f"{slug}.meta.json"
    frames_dir = out_root / "frames" / slug
    md_path.parent.mkdir(parents=True, exist_ok=True)

    sha = video_sha(video)
    if md_path.exists() and meta_path.exists():
        try:
            existing = json.loads(meta_path.read_text())
            if existing.get("sha256") == sha:
                return False
        except Exception:
            pass

    frames = extract_frames(video, frames_dir, n_frames, width)
    if not frames:
        print(f"[skip] no frames extracted from {video.name}", file=sys.stderr)
        return False

    duration = probe_duration(video)
    lines: list[str] = [
        f"# Video: {video.name}",
        "",
        f"Source: declassified DoD/UAP video",
        f"Duration: {duration:.1f}s",
        f"Frames sampled: {len(frames)}",
        "",
    ]
    for i, frame in enumerate(frames):
        ts = (i + 0.5) * duration / len(frames)
        desc = describe_frame(client, endpoint, model, frame)
        lines.append(f"## Frame {i:02d} @ {ts:.2f}s")
        lines.append("")
        lines.append(desc or "(no description)")
        lines.append("")

    md_path.write_text("\n".join(lines))
    meta_path.write_text(json.dumps({
        "doc_id": slug,
        "source_path": str(video),
        "sha256": sha,
        "sha8": sha[:8],
        "filename": video.name,
        "title": video.stem.replace("_", " "),
        "char_count": sum(len(l) for l in lines),
        "media_type": "video",
        "duration_sec": duration,
        "frame_count": len(frames),
    }, indent=2))
    return True


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="in_dir", required=True)
    ap.add_argument("--out", dest="out_dir", required=True)
    ap.add_argument("--endpoint", default="http://localhost:8080")
    ap.add_argument("--model", default="unsloth/Qwen3.6-35B-A3B-GGUF:UD-Q8_K_XL")
    ap.add_argument("--frames", type=int, default=6, help="Frames per video")
    ap.add_argument("--width", type=int, default=768)
    ap.add_argument("--limit", type=int, default=0)
    args = ap.parse_args()

    if shutil.which("ffmpeg") is None or shutil.which("ffprobe") is None:
        print("ffmpeg/ffprobe must be on PATH", file=sys.stderr)
        return 1

    in_dir = Path(args.in_dir)
    out_root = Path(args.out_dir)
    videos = sorted(p for p in in_dir.rglob("*.mp4") if "__MACOSX" not in p.parts)
    if args.limit:
        videos = videos[: args.limit]
    if not videos:
        print(f"No mp4 files under {in_dir}", file=sys.stderr)
        return 1

    done = 0
    skipped = 0
    with httpx.Client() as client:
        for v in tqdm(videos, desc="video"):
            try:
                if process_video(v, out_root, client, args.endpoint, args.model,
                                 args.frames, args.width):
                    done += 1
                else:
                    skipped += 1
            except Exception as e:
                print(f"[fail] {v.name}: {e}", file=sys.stderr)

    print(f"Done. described={done} skipped={skipped} total={len(videos)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
