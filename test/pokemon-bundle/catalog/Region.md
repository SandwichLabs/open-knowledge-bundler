---
type: Node Type
title: Region
description: Node type "Region" — 3 current instances in the pokemon_world graph.
tags:
    - schema
    - node-type
node_type: Region
---

# Fields

| Field | Key | Source |
|-------|-----|--------|
| `node_id` | ✓ | `region_name` |
| `name` |  | `region_name` |

# Semantic Fields

Fields concatenated to form the embedding text:

* `name`
* `description`

# Relationships

* **Incoming:** [FOUND_IN](rel_FOUND_IN.md)

# Examples

* [region:hokkaido](/Region/region_hokkaido.md)
* [region:johto](/Region/region_johto.md)
* [region:kanto](/Region/region_kanto.md)

