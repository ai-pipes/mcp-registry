package domain

import "slices"

// Ranked is an id with its fused score.
type Ranked struct {
	ID    string
	Score float64
}

// RRF is reciprocal rank fusion: score(id) = sum over the lists of 1 / (k + rank), ranks 1-based,
// summed in list order. An id counts once per list (its first position; a repeat still takes a
// rank). The result is sorted by descending score; ties keep first-seen order (the first list
// first, then ids that appear only later).
func RRF(lists [][]string, k int) []Ranked {
	scores := map[string]float64{}
	var order []string
	for _, list := range lists {
		seen := map[string]bool{}
		for i, id := range list {
			if seen[id] {
				continue
			}
			seen[id] = true
			prev, ok := scores[id]
			if !ok {
				order = append(order, id)
			}
			scores[id] = prev + 1/float64(k+i+1)
		}
	}
	out := make([]Ranked, 0, len(order))
	for _, id := range order {
		out = append(out, Ranked{ID: id, Score: scores[id]})
	}
	slices.SortStableFunc(out, func(a, b Ranked) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	return out
}

// Candidate is one row of a candidate list: a tool ref and the server (slug) of its document.
type Candidate struct {
	Ref    string
	Server string
}

// ToolHit is a search result; Score is in (0, 1].
type ToolHit struct {
	Ref    string
	Server string
	Score  float64
}

// Fuse fuses the full-text and the vector candidate lists (each already ordered; vector may be
// nil when it was not used, which gives the same scores as an empty one), keeps the first limit
// and scales each score by the best possible fused score over Lists lists:
// score = rrf / (Lists / (RRFK + 1)), a fixed divisor, never rescaled to the best hit. The server of
// a hit is that of its candidate row (the vector row's when both lists have the ref).
func Fuse(fts, vector []Candidate, limit int) []ToolHit {
	serverOf := make(map[string]string, len(fts)+len(vector))
	refs := func(cs []Candidate) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			serverOf[c.Ref] = c.Server
			out[i] = c.Ref
		}
		return out
	}
	lists := [][]string{refs(fts), refs(vector)}
	maxScore := float64(Lists) / float64(RRFK+1)
	fused := RRF(lists, RRFK)
	if limit < len(fused) {
		fused = fused[:max(limit, 0)]
	}
	hits := make([]ToolHit, 0, len(fused))
	for _, f := range fused {
		if server, ok := serverOf[f.ID]; ok {
			hits = append(hits, ToolHit{Ref: f.ID, Server: server, Score: f.Score / maxScore})
		}
	}
	return hits
}
