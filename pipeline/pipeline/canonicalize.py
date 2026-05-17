"""Entity canonicalization for the UFO KG.

Two-pass model:
  1. `feed(etype, name)` — collect every surface form seen.
  2. `build()` — compute a {raw_surface -> canonical_surface or None} map per type.
  3. `resolve(etype, name)` — return canonical (or None to drop).

Person merging strategy
-----------------------
Tokenize stripping titles (Mr., Captain, Lt., Queen, ...). Last token is surname,
first token's first letter is the initial. Group by lowercase surname.

Within a surname group:
  * "rich" entries (>=2 tokens) cluster by (initial, surname).
  * "bare" entries (single token == the surname) merge into the rich cluster IF
    exactly one (initial, surname) cluster exists for that surname; otherwise
    they stay separate (ambiguous).
  * The canonical name for a cluster is the longest surface form (most tokens,
    then most chars) seen — preserves the richest published spelling.

Stop-list filters pronouns, single-letter "names", and obvious extraction garbage.

Location / Object / Agency merging
----------------------------------
Casefold, strip punctuation, collapse whitespace, and drop a trailing plural 's'.
Cluster by normalized key; canonical is the most-frequent original spelling.

Dates are passed through unchanged (handled separately in build_ndjson).
"""
from __future__ import annotations

import re
from collections import Counter, defaultdict

TITLES = {
    "mr", "mrs", "ms", "miss", "dr", "doctor",
    "capt", "captain", "lt", "lieutenant", "col", "colonel",
    "gen", "general", "sgt", "sergeant", "maj", "major",
    "cmdr", "commander", "adm", "admiral", "pvt", "private",
    "cpl", "corporal", "sir", "lady", "lord",
    "queen", "king", "prince", "princess", "president",
    "prof", "professor", "rev", "reverend",
    "the", "a", "an",
}

PERSON_STOP = {
    # Pronouns and other tokens the LLM occasionally returns as "Person".
    "i", "we", "he", "she", "it", "you", "they", "them", "us", "me",
    "one", "someone", "anyone", "everybody", "nobody", "everyone",
    "this", "that", "these", "those",
    "unknown", "n/a", "na", "none", "null",
    "redacted", "redaction", "name", "subject", "witness",
}

GENERIC_OBJECT_STOP = {"object", "unknown", "n/a", "none", "thing", "it"}
GENERIC_LOC_STOP = {"unknown", "n/a", "none", "here", "there", "somewhere"}

_PUNCT_RE = re.compile(r"[^\w\s]")
_WS_RE = re.compile(r"\s+")

# Mid-word punctuation that signals OCR damage (e.g. "J'oh.n", "Geor.g.e").
_MIDWORD_PUNCT_RE = re.compile(r"\w[.\'`]\w")
_LEADING_TITLE_RE = re.compile(
    r"^(?:mr|mrs|ms|miss|dr|doctor|capt|captain|lt|lieutenant|col|colonel|"
    r"gen|general|sgt|sergeant|maj|major|cmdr|commander|adm|admiral|"
    r"pvt|private|cpl|corporal|sir|lady|lord|queen|king|prince|princess|"
    r"president|prof|professor|rev|reverend|defense\s+secretary)\.?\s+",
    re.IGNORECASE,
)


def _strip_leading_title(name: str) -> str:
    """Drop a single leading title from a display name (titles can still live in `role`)."""
    return _LEADING_TITLE_RE.sub("", name).strip() or name


def _quality_score(name: str, count: int) -> tuple:
    """Higher tuple = better canonical pick. Penalises OCR mangling and ALL CAPS."""
    midword_pen = len(_MIDWORD_PUNCT_RE.findall(name))
    allcaps_pen = 1 if (name.isupper() and len(name) > 3) else 0
    # Title Case = at least one lowercase + one uppercase letter present.
    titlecase = 1 if (any(c.islower() for c in name) and any(c.isupper() for c in name)) else 0
    token_count = len(name.split())
    # Sort key: clean > title-cased > frequent > token-rich > shorter (cleaner).
    return (-midword_pen, -allcaps_pen, titlecase, count, token_count, -len(name))


def _strip_titles_tokens(name: str) -> list[str]:
    """Lowercase tokens with titles and punctuation removed. Empty list if all-titles."""
    cleaned = _PUNCT_RE.sub(" ", name)
    cleaned = _WS_RE.sub(" ", cleaned).strip().lower()
    if not cleaned:
        return []
    return [t for t in cleaned.split(" ") if t and t not in TITLES]


def _norm_simple(name: str) -> str:
    """Casefold + strip punct + collapse whitespace + de-pluralize."""
    s = _PUNCT_RE.sub(" ", name)
    s = _WS_RE.sub(" ", s).strip().lower()
    if len(s) > 3 and s.endswith("s") and not s.endswith("ss"):
        s = s[:-1]
    return s


class Canonicalizer:
    def __init__(self) -> None:
        self._raw_counts: dict[str, Counter] = {  # etype -> Counter[raw_name]
            "Person": Counter(),
            "Location": Counter(),
            "Object": Counter(),
            "Agency": Counter(),
        }
        self._resolved: dict[str, dict[str, str | None]] = {}
        self.dropped: Counter = Counter()  # by-reason

    def feed(self, etype: str, name: str) -> None:
        if etype not in self._raw_counts or not name:
            return
        self._raw_counts[etype][name.strip()] += 1

    def build(self) -> None:
        self._resolved["Person"] = self._build_persons()
        for et in ("Location", "Object", "Agency"):
            self._resolved[et] = self._build_simple(et)

    # -------- Person ---------------------------------------------------------

    def _build_persons(self) -> dict[str, str | None]:
        out: dict[str, str | None] = {}
        counts = self._raw_counts["Person"]

        # Phase 1: classify each surface form.
        # entry = (raw, count, tokens, surname, initial, length_key)
        entries = []
        for raw, n in counts.items():
            toks = _strip_titles_tokens(raw)
            if not toks:
                out[raw] = None
                self.dropped["person_empty_after_titles"] += n
                continue
            if all(t in PERSON_STOP for t in toks):
                out[raw] = None
                self.dropped["person_stopword"] += n
                continue
            if len(toks) == 1 and len(toks[0]) <= 1:
                out[raw] = None
                self.dropped["person_single_letter"] += n
                continue
            surname = toks[-1]
            initial = toks[0][0] if len(toks) >= 2 else None
            entries.append((raw, n, toks, surname, initial))

        # Phase 2: build surname groups.
        by_surname: dict[str, list] = defaultdict(list)
        for e in entries:
            by_surname[e[3]].append(e)

        for surname, group in by_surname.items():
            rich = [e for e in group if e[4] is not None]   # initial known
            bare = [e for e in group if e[4] is None]       # surname only

            # Cluster rich entries by (initial, surname).
            clusters: dict[str | None, list] = defaultdict(list)
            for e in rich:
                clusters[e[4]].append(e)

            cluster_canon: dict[str | None, str] = {}
            cluster_count: dict[str | None, int] = {}
            for init, members in clusters.items():
                # Canonical surface = best-quality form (clean > title-cased > frequent).
                members.sort(key=lambda e: _quality_score(e[0], e[1]), reverse=True)
                pick = _strip_leading_title(members[0][0])
                cluster_canon[init] = pick
                cluster_count[init] = sum(m[1] for m in members)
                for e in members:
                    out[e[0]] = pick

            # Bare-surname disambiguation.
            if not clusters:
                # No rich entries — pick the best bare form as canonical for the surname.
                bare.sort(key=lambda e: _quality_score(e[0], e[1]), reverse=True)
                if bare:
                    canon = _strip_leading_title(bare[0][0])
                    for e in bare:
                        out[e[0]] = canon
            elif len(clusters) == 1:
                only = next(iter(cluster_canon.values()))
                for e in bare:
                    out[e[0]] = only
            else:
                # Multiple rich clusters share this surname. If one dominates (>=5x
                # the runner-up by mention count), merge bare entries into it; else
                # keep bare entries ambiguous (as themselves).
                ranked = sorted(cluster_count.items(), key=lambda kv: kv[1], reverse=True)
                top_init, top_n = ranked[0]
                runner_n = ranked[1][1] if len(ranked) > 1 else 0
                if runner_n == 0 or top_n >= 5 * runner_n:
                    for e in bare:
                        out[e[0]] = cluster_canon[top_init]
                else:
                    for e in bare:
                        out[e[0]] = e[0]

        return out

    # -------- Location / Object / Agency -------------------------------------

    def _build_simple(self, etype: str) -> dict[str, str | None]:
        stop = {
            "Location": GENERIC_LOC_STOP,
            "Object":   GENERIC_OBJECT_STOP,
            "Agency":   {"unknown", "n/a", "none"},
        }[etype]

        out: dict[str, str | None] = {}
        by_key: dict[str, list[tuple[str, int]]] = defaultdict(list)
        for raw, n in self._raw_counts[etype].items():
            key = _norm_simple(raw)
            if not key or key in stop:
                out[raw] = None
                self.dropped[f"{etype.lower()}_filtered"] += n
                continue
            if len(key) <= 1:
                out[raw] = None
                self.dropped[f"{etype.lower()}_too_short"] += n
                continue
            by_key[key].append((raw, n))

        for key, members in by_key.items():
            # Canonical = best-quality form (clean > title-cased > frequent).
            members.sort(key=lambda x: _quality_score(x[0], x[1]), reverse=True)
            canon = members[0][0]
            for raw, _ in members:
                out[raw] = canon
        return out

    # -------- Lookup ---------------------------------------------------------

    def resolve(self, etype: str, name: str) -> str | None:
        """Return canonical name, or None if this form should be dropped."""
        if etype not in self._resolved:
            return name  # types we don't canonicalize (Date) pass through
        return self._resolved[etype].get(name.strip(), name.strip())

    def stats(self) -> dict:
        merges: dict[str, dict[str, int]] = {}
        for et, mp in self._resolved.items():
            kept = sum(1 for v in mp.values() if v is not None)
            dropped = sum(1 for v in mp.values() if v is None)
            unique_canon = len({v for v in mp.values() if v})
            merges[et] = {
                "raw_surfaces": len(mp),
                "dropped": dropped,
                "kept": kept,
                "canonical_unique": unique_canon,
                "merge_ratio": round(kept / unique_canon, 2) if unique_canon else 0.0,
            }
        return {"by_type": merges, "drop_reasons": dict(self.dropped)}
