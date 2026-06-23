package extract

import "strings"

// Chunk is a slice of the corpus with its ordinal id (used for provenance).
type Chunk struct {
	ID   int
	Text string
}

// ChunkText splits text into overlapping, sentence-aware chunks of roughly size
// characters. It prefers to break on a sentence boundary (". ") in the back
// half of each window so facts are less likely to be split mid-sentence;
// overlap re-includes the tail of the previous chunk to catch facts that
// straddle a boundary.
func ChunkText(text string, size, overlap int) []Chunk {
	if size <= 0 {
		size = 6000
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}

	var chunks []Chunk
	i, n := 0, len(text)
	for i < n {
		end := i + size
		if end >= n {
			end = n
		} else {
			// Prefer a sentence boundary in the back half of the window.
			if m := strings.LastIndex(text[i+size/2:end], ". "); m != -1 {
				end = i + size/2 + m + 1
			}
		}
		seg := strings.TrimSpace(text[i:end])
		if seg != "" {
			chunks = append(chunks, Chunk{ID: len(chunks), Text: seg})
		}
		if end >= n {
			break
		}
		next := end - overlap
		if next <= i {
			next = i + 1
		}
		i = next
	}
	return chunks
}

// SampleChunks returns up to k chunks spread evenly across cs (for ontology
// bootstrap — a representative slice of the corpus without reading all of it).
func SampleChunks(cs []Chunk, k int) []Chunk {
	if k <= 0 || len(cs) <= k {
		return cs
	}
	out := make([]Chunk, 0, k)
	step := float64(len(cs)) / float64(k)
	for i := 0; i < k; i++ {
		out = append(out, cs[int(float64(i)*step)])
	}
	return out
}
