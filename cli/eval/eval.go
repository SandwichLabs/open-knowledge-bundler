// Package eval scores a local agent's free-form answers against a known-answer
// key. It is deliberately deterministic (no LLM judge): for knowledge-graph QA
// the gold answer is a set of entity names, so coverage and over-generation can
// be measured by string containment.
package eval

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// Question is one graded item from a questions.jsonl file. gold is the set of
// acceptable answer strings (entity names); an answer is credited with a gold
// item when that item appears in the answer text.
type Question struct {
	ID       string            `json:"id,omitempty"`
	Question string            `json:"question"`
	Gold     []string          `json:"gold"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// Score is the deterministic grade for one answered question.
type Score struct {
	// GoldFound / GoldTotal drive recall: how much of the answer key the
	// response covered.
	GoldFound int `json:"gold_found"`
	GoldTotal int `json:"gold_total"`
	// Recall = GoldFound / GoldTotal.
	Recall float64 `json:"recall"`
	// Exact is true when every gold item was found (Hits@all) — the headline
	// correctness signal for closed-answer KGQA.
	Exact bool `json:"exact"`

	// Precision metrics require a vocabulary (the domain's full entity-name
	// set). VocabAware is false when no vocab was supplied, in which case
	// Precision/F1 are not meaningful and should be ignored.
	VocabAware bool `json:"vocab_aware"`
	// VocabMentioned is the count of distinct known entities the answer named;
	// Precision = GoldFound / VocabMentioned catches over-generation (naming
	// real entities that are not in the gold set — the hallucination mode).
	VocabMentioned int     `json:"vocab_mentioned,omitempty"`
	Precision      float64 `json:"precision,omitempty"`
	F1             float64 `json:"f1,omitempty"`

	// HonestMiss is true when the answer covered no gold items but explicitly
	// disclaimed (e.g. "not found in the graph") rather than asserting a wrong
	// answer. An honest miss is a categorically better failure than a confident
	// wrong answer, so the leaderboard tracks the two separately.
	HonestMiss bool `json:"honest_miss"`
}

// ReadQuestions parses a questions.jsonl stream (one Question per line; blank
// lines and lines beginning with # are skipped).
func ReadQuestions(r io.Reader) ([]Question, error) {
	var out []Question
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var q Question
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, sc.Err()
}

var disclaimerRe = regexp.MustCompile(`(?i)\b(not found|no (records|results|matches|data|rows)|couldn'?t find|could not find|i (don'?t|do not) (have|know)|no .* (in|found in) the (graph|database|data)|unable to find|nothing (found|matching))\b`)

// Grade scores one answer against a question's gold set. vocab, if non-empty,
// is the domain's full set of entity names (lower-cased) used for precision;
// pass nil to score recall/exact only.
func Grade(answer string, q Question, vocab map[string]struct{}) Score {
	norm := normalize(answer)
	var s Score
	s.GoldTotal = len(q.Gold)

	goldSet := make(map[string]struct{}, len(q.Gold))
	for _, g := range q.Gold {
		gn := normalize(g)
		if gn == "" {
			s.GoldTotal-- // ignore empty gold entries
			continue
		}
		goldSet[gn] = struct{}{}
		if containsPhrase(norm, gn) {
			s.GoldFound++
		}
	}
	if s.GoldTotal > 0 {
		s.Recall = float64(s.GoldFound) / float64(s.GoldTotal)
	}
	s.Exact = s.GoldTotal > 0 && s.GoldFound == s.GoldTotal

	if len(vocab) > 0 {
		s.VocabAware = true
		// Count distinct known entities the answer mentions. Gold items are
		// part of the vocabulary, so this includes the true positives plus any
		// other real entities the model named (the false positives). Entities
		// named in the question itself (the topic/subject) are excluded — the
		// model restating them is not over-generation.
		qnorm := normalize(q.Question)
		mentioned := map[string]struct{}{}
		for name := range vocab {
			if _, isGold := goldSet[name]; !isGold && containsPhrase(qnorm, name) {
				continue // topic entity from the question, not a claimed answer
			}
			if containsPhrase(norm, name) {
				mentioned[name] = struct{}{}
			}
		}
		// Ensure gold items found in the answer are always counted as mentioned,
		// even if the supplied vocab is incomplete.
		for gn := range goldSet {
			if containsPhrase(norm, gn) {
				mentioned[gn] = struct{}{}
			}
		}
		s.VocabMentioned = len(mentioned)
		if s.VocabMentioned > 0 {
			s.Precision = float64(s.GoldFound) / float64(s.VocabMentioned)
		}
		if s.Precision+s.Recall > 0 {
			s.F1 = 2 * s.Precision * s.Recall / (s.Precision + s.Recall)
		}
	}

	if s.GoldFound == 0 && disclaimerRe.MatchString(answer) {
		s.HonestMiss = true
	}
	return s
}

// NormalizeVocab lower-cases and trims a list of entity names into the set form
// Grade expects, dropping very short (<2 char) entries that would match noise.
func NormalizeVocab(names []string) map[string]struct{} {
	v := make(map[string]struct{}, len(names))
	for _, n := range names {
		nn := normalize(n)
		if len(nn) >= 2 {
			v[nn] = struct{}{}
		}
	}
	return v
}

var spaceRe = regexp.MustCompile(`\s+`)

// normalize lower-cases, strips most punctuation to spaces, and collapses
// whitespace so containment matching is robust to formatting (markdown
// bullets, commas, quoting) without being defeated by it.
func normalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n':
			b.WriteRune(' ')
		default:
			// Map punctuation/symbols to a space so "spider-man" ~ "spider man".
			b.WriteRune(' ')
		}
	}
	return strings.TrimSpace(spaceRe.ReplaceAllString(b.String(), " "))
}

// containsPhrase reports whether needle appears in haystack as a whole-token
// run (both are already normalized to space-separated lower-case tokens). This
// avoids "Ash" matching inside "Ashley" while still matching multi-word names.
func containsPhrase(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	h := " " + haystack + " "
	n := " " + needle + " "
	return strings.Contains(h, n)
}
