package extract

import (
	"context"
	"fmt"
	"sort"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/domain"
)

// NormalizeReport summarizes the relation-normalization pass for reporting.
type NormalizeReport struct {
	Inverted       int      // edges rewritten from an inverse relation (endpoints swapped)
	Flipped        int      // edges flipped to match declared source->target types
	OffVocabMapped int      // off-vocabulary relations mapped to a canonical one
	OffVocabBucket int      // off-vocabulary relations dropped into the OTHER bucket
	TypeMismatch   int      // edges whose endpoint types matched neither orientation (kept, logged)
	RelationVocab  []string // final distinct relation names
}

const otherRelation = "RELATED_TO"

// Normalize performs stage 4: rewrite every relation to a canonical ontology
// relation with the declared direction. Because extraction is enum-constrained
// the relations are already canonical in the common case; this fixes inverse
// phrasings, reversed endpoints, and any off-vocabulary drift (e.g. from a
// hand-edited ontology), with no silent loss — everything is counted.
func Normalize(ctx context.Context, gen *Generator, ont *domain.Ontology, res *Resolved, maxMap int, progress ProgressFunc) (*NormalizeReport, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}

	canonical := map[string]domain.RelationDef{}
	for _, r := range ont.Relations {
		canonical[r.Name] = r
	}
	// Build the inverse map AFTER the canonical set: skip self-inverses and any
	// inverse name that is itself a canonical relation (a name that is both
	// canonical and someone's inverse must stay canonical, not get rewritten).
	inverse := map[string]string{} // inverse-name -> canonical-name
	for _, r := range ont.Relations {
		if r.Inverse == "" || r.Inverse == r.Name {
			continue
		}
		if _, isCanonical := canonical[r.Inverse]; isCanonical {
			continue
		}
		inverse[r.Inverse] = r.Name
	}

	rep := &NormalizeReport{}
	offVocabCache := map[string]string{} // raw relation -> mapped canonical (or "" = bucket)
	mapped := 0

	out := make([]ResolvedRelation, 0, len(res.Relations))
	for _, rel := range res.Relations {
		name := rel.Relation
		src, tgt := rel.SourceID, rel.TargetID

		// A canonical relation is kept as-is. Only a non-canonical name is
		// considered for inverse rewriting, then off-vocabulary mapping.
		if _, isCanonical := canonical[name]; !isCanonical {
			if canon, ok := inverse[name]; ok {
				// Inverse phrasing: rewrite to canonical and swap endpoints.
				name = canon
				src, tgt = tgt, src
				rep.Inverted++
			}
		}

		// Off-vocabulary: map to nearest canonical or bucket into OTHER.
		if _, ok := canonical[name]; !ok && name != otherRelation {
			cached, seen := offVocabCache[name]
			if !seen {
				cached = ""
				if maxMap == 0 || mapped < maxMap {
					mapped++
					cached = mapRelation(ctx, gen, ont, name)
				}
				offVocabCache[name] = cached
			}
			if cached != "" {
				name = cached
				rep.OffVocabMapped++
			} else {
				name = otherRelation
				rep.OffVocabBucket++
			}
		}

		// Direction validation against declared source_type -> target_type.
		if def, ok := canonical[name]; ok {
			sNode := res.NodeByID[src]
			tNode := res.NodeByID[tgt]
			if sNode != nil && tNode != nil && def.SourceType != "" && def.TargetType != "" &&
				def.SourceType != "Other" && def.TargetType != "Other" {
				matches := typeMatch(sNode.Type, def.SourceType) && typeMatch(tNode.Type, def.TargetType)
				reversed := typeMatch(sNode.Type, def.TargetType) && typeMatch(tNode.Type, def.SourceType)
				switch {
				case matches:
					// good
				case reversed:
					src, tgt = tgt, src
					rep.Flipped++
				default:
					rep.TypeMismatch++ // kept (ontology types may be imperfect); just counted
				}
			}
		}

		out = append(out, ResolvedRelation{SourceID: src, Relation: name, TargetID: tgt, Chunks: rel.Chunks})
	}

	// Dedup after rewrites (inverse swaps / flips can collide).
	seen := map[string]*ResolvedRelation{}
	for i := range out {
		r := out[i]
		if r.SourceID == r.TargetID {
			continue
		}
		key := r.SourceID + "|" + r.Relation + "|" + r.TargetID
		if ex, ok := seen[key]; ok {
			ex.Chunks = mergeInts(ex.Chunks, r.Chunks)
			continue
		}
		rr := r
		seen[key] = &rr
	}

	final := make([]ResolvedRelation, 0, len(seen))
	vocab := map[string]bool{}
	for _, r := range seen {
		final = append(final, *r)
		vocab[r.Relation] = true
	}
	sort.Slice(final, func(i, j int) bool {
		if final[i].SourceID != final[j].SourceID {
			return final[i].SourceID < final[j].SourceID
		}
		if final[i].Relation != final[j].Relation {
			return final[i].Relation < final[j].Relation
		}
		return final[i].TargetID < final[j].TargetID
	})
	res.Relations = final

	rep.RelationVocab = sortedKeys(vocab)
	if rep.OffVocabBucket > 0 {
		progress("  %d relation(s) had no canonical match and went to the %s bucket", rep.OffVocabBucket, otherRelation)
	}
	if rep.TypeMismatch > 0 {
		progress("  %d edge(s) matched neither declared orientation (kept; review ontology types)", rep.TypeMismatch)
	}
	return rep, nil
}

func typeMatch(actual, declared string) bool {
	return actual == declared || actual == "Other" || declared == "Other"
}

// mapRelation asks the model to map an off-vocabulary relation to the nearest
// canonical relation (enum-constrained), or returns "" to bucket it.
func mapRelation(ctx context.Context, gen *Generator, ont *domain.Ontology, name string) string {
	choices := append([]string{}, ont.RelationNames()...)
	choices = append(choices, "NONE")
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"relation": map[string]any{"type": "string", "enum": toAny(choices)},
		},
		"required": []string{"relation"},
	}
	var out struct {
		Relation string `json:"relation"`
	}
	user := fmt.Sprintf("Map the relation %q to the closest one from the list, or NONE if none fits.", name)
	if _, err := gen.GenerateJSON(ctx, "You map a relation name to the closest canonical relation.", user, schema, &out); err != nil {
		return ""
	}
	if out.Relation == "NONE" {
		return ""
	}
	return out.Relation
}
