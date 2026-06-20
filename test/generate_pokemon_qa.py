#!/usr/bin/env python3
"""Generate a full, ground-truthed QA set from the Pokemon knowledge graph.

Templated over every relation (the same way MetaQA was built from WikiMovies),
with gold answers pulled directly from DuckDB so the answer key is exact. Each
question is tagged by `mode` (the retrieval pattern) and `dir` (forward vs
reverse graph traversal) so the eval leaderboard can break results down.

Usage:
  python3 generate_pokemon_qa.py [--cbi ../cli/cbi] [--config domain.yaml] > pokemon-qa.jsonl
"""
import argparse
import json
import subprocess
import sys


def disp(alias):
    """Display-name expression: node types key their name differently
    (Pokemon->name, Type->type_name, Region->region_name, Trainer->trainer_name)."""
    return ("coalesce({a}.properties->>'name', {a}.properties->>'type_name', "
            "{a}.properties->>'region_name', {a}.properties->>'trainer_name')").format(a=alias)


def q(cbi, config, sql):
    """Run a read-only graph query and return rows as a list of dicts."""
    out = subprocess.run(
        [cbi, "graph", "--config", config, "--sql", sql],
        capture_output=True, text=True,
    )
    if out.returncode != 0:
        sys.exit(f"query failed: {out.stderr}\nSQL: {sql}")
    return json.loads(out.stdout or "[]")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--cbi", default="../cli/cbi")
    ap.add_argument("--config", default="domain.yaml")
    args = ap.parse_args()
    cbi, config = args.cbi, args.config

    rows = []
    n = 0

    def emit(mode, direction, question, gold):
        nonlocal n
        gold = sorted({g for g in gold if g})
        if not gold:
            return
        rows.append({
            "id": f"{mode}-{direction}-{n}",
            "question": question,
            "gold": gold,
            "tags": {"mode": mode, "dir": direction},
        })
        n += 1

    # --- Forward single-hop: Pokemon -> attribute ---
    for r in q(cbi, config, f"""
        SELECT {disp('p')} AS pokemon, list({disp('t')}) AS vals
        FROM Edges_Base e
        JOIN Nodes_Base p ON e.source_id = p.node_id
        JOIN Nodes_Base t ON e.target_id = t.node_id
        WHERE e.is_current AND e.relationship_type = 'HAS_TYPE'
        GROUP BY 1 ORDER BY 1"""):
        emit("types", "forward", f"What type or types is {r['pokemon']}?", r["vals"])

    for r in q(cbi, config, f"""
        SELECT {disp('p')} AS pokemon, list({disp('g')}) AS vals
        FROM Edges_Base e
        JOIN Nodes_Base p ON e.source_id = p.node_id
        JOIN Nodes_Base g ON e.target_id = g.node_id
        WHERE e.is_current AND e.relationship_type = 'FOUND_IN'
        GROUP BY 1 ORDER BY 1"""):
        emit("region", "forward", f"In which region is {r['pokemon']} found?", r["vals"])

    for r in q(cbi, config, f"""
        SELECT {disp('p')} AS pokemon, list({disp('tr')}) AS vals
        FROM Edges_Base e
        JOIN Nodes_Base p ON e.source_id = p.node_id
        JOIN Nodes_Base tr ON e.target_id = tr.node_id
        WHERE e.is_current AND e.relationship_type = 'OWNED_BY'
        GROUP BY 1 ORDER BY 1"""):
        emit("owner", "forward", f"Which trainer owns {r['pokemon']}?", r["vals"])

    # --- Forward single-hop: direct evolution ---
    evo_pairs = q(cbi, config, f"""
        SELECT {disp('a')} AS src, {disp('b')} AS dst
        FROM Edges_Base e
        JOIN Nodes_Base a ON e.source_id = a.node_id
        JOIN Nodes_Base b ON e.target_id = b.node_id
        WHERE e.is_current AND e.relationship_type = 'EVOLVES_TO'""")
    nxt = {}
    for r in evo_pairs:
        nxt.setdefault(r["src"], []).append(r["dst"])
        emit("evolve", "forward", f"What does {r['src']} evolve into?", [r["dst"]])

    # --- Reverse single-hop: attribute -> Pokemon (the traversal that's hard) ---
    for r in q(cbi, config, f"""
        SELECT {disp('t')} AS attr, list({disp('p')}) AS vals
        FROM Edges_Base e
        JOIN Nodes_Base p ON e.source_id = p.node_id
        JOIN Nodes_Base t ON e.target_id = t.node_id
        WHERE e.is_current AND e.relationship_type = 'HAS_TYPE'
        GROUP BY 1 ORDER BY 1"""):
        emit("by_type", "reverse", f"Which Pokemon are {r['attr']} type?", r["vals"])

    for r in q(cbi, config, f"""
        SELECT {disp('g')} AS attr, list({disp('p')}) AS vals
        FROM Edges_Base e
        JOIN Nodes_Base p ON e.source_id = p.node_id
        JOIN Nodes_Base g ON e.target_id = g.node_id
        WHERE e.is_current AND e.relationship_type = 'FOUND_IN'
        GROUP BY 1 ORDER BY 1"""):
        emit("by_region", "reverse", f"Which Pokemon are found in the {r['attr']} region?", r["vals"])

    for r in q(cbi, config, f"""
        SELECT {disp('tr')} AS attr, list({disp('p')}) AS vals
        FROM Edges_Base e
        JOIN Nodes_Base p ON e.source_id = p.node_id
        JOIN Nodes_Base tr ON e.target_id = tr.node_id
        WHERE e.is_current AND e.relationship_type = 'OWNED_BY'
        GROUP BY 1 ORDER BY 1"""):
        emit("by_trainer", "reverse", f"Which Pokemon does {r['attr']} own?", r["vals"])

    # --- Multi-hop: full evolution line from each base form ---
    has_parent = {p for ds in nxt.values() for p in ds}
    bases = [s for s in nxt if s not in has_parent]
    for base in sorted(bases):
        chain, cur = [base], base
        while cur in nxt:
            cur = sorted(nxt[cur])[0]
            chain.append(cur)
        if len(chain) >= 2:
            emit("evo_line", "multihop", f"What is the full evolution line starting from {base}?", chain)

    # --- Aggregation ---
    counts = {r["node_type"]: r["c"] for r in q(cbi, config,
        "SELECT node_type, count(*) AS c FROM Nodes_Base WHERE is_current GROUP BY 1")}
    for nt, c in counts.items():
        emit("count", "aggregation", f"How many nodes of type {nt} are in the graph?", [str(c)])

    for row in rows:
        print(json.dumps(row, ensure_ascii=False))
    print(f"generated {len(rows)} questions", file=sys.stderr)


if __name__ == "__main__":
    main()
