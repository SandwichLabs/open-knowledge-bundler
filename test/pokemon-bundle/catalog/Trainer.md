---
type: Node Type
title: Trainer
description: Node type "Trainer" — 5 current instances in the pokemon_world graph.
tags:
    - schema
    - node-type
node_type: Trainer
---

# Fields

| Field | Key | Source |
|-------|-----|--------|
| `node_id` | ✓ | `trainer_name` |
| `name` |  | `trainer_name` |

# Semantic Fields

Fields concatenated to form the embedding text:

* `name`
* `specialty`

# Relationships

* **Incoming:** [OWNED_BY](rel_OWNED_BY.md)

# Examples

* [trainer:ash](/Trainer/trainer_ash.md)
* [trainer:brock](/Trainer/trainer_brock.md)
* [trainer:gary](/Trainer/trainer_gary.md)
* [trainer:giovanni](/Trainer/trainer_giovanni.md)
* [trainer:misty](/Trainer/trainer_misty.md)

