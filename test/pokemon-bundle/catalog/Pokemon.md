---
type: Node Type
title: Pokemon
description: Node type "Pokemon" — 20 current instances in the pokemon_world graph.
tags:
    - schema
    - node-type
node_type: Pokemon
---

# Fields

| Field | Key | Source |
|-------|-----|--------|
| `node_id` | ✓ | `pokedex_id` |
| `name` |  | `name` |
| `generation` |  | `generation` |

# Semantic Fields

Fields concatenated to form the embedding text:

* `name`
* `description`

# Relationships

* **Outgoing:** [EVOLVES_TO](rel_EVOLVES_TO.md), [FOUND_IN](rel_FOUND_IN.md), [HAS_TYPE](rel_HAS_TYPE.md), [OWNED_BY](rel_OWNED_BY.md)
* **Incoming:** [EVOLVES_TO](rel_EVOLVES_TO.md)

# Examples

* [Bulbasaur](/Pokemon/pokemon_001.md)
* [Ivysaur](/Pokemon/pokemon_002.md)
* [Venusaur](/Pokemon/pokemon_003.md)
* [Charmander](/Pokemon/pokemon_004.md)
* [Charmeleon](/Pokemon/pokemon_005.md)
* [Charizard](/Pokemon/pokemon_006.md)
* [Squirtle](/Pokemon/pokemon_007.md)
* [Wartortle](/Pokemon/pokemon_008.md)
* [Blastoise](/Pokemon/pokemon_009.md)
* [Pikachu](/Pokemon/pokemon_025.md)

