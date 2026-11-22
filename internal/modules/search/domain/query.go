package domain

import "github.com/ai-pipes/mcp-registry/internal/kernel"

const (
	// Candidates is the length of each candidate list before fusion.
	Candidates = 50
	// RRFK is the constant of reciprocal rank fusion.
	RRFK = 60
	// Lists is the number of candidate lists a perfect hit tops (full text and vector).
	Lists = 2
	// DefaultMinSimilarity is the default cosine similarity a vector candidate must reach
	// (calibrated on multilingual-e5-small q8: unrelated queries <= 0.809, relevant >= 0.859).
	DefaultMinSimilarity = 0.83
)

// PrepareQuery trims the query as JavaScript's trim() does. ok is false (nothing to search, no
// index and no embedder call) for a blank query, a limit <= 0 or no servers to search in.
func PrepareQuery(query string, limit int, servers []string) (trimmed string, ok bool) {
	trimmed = kernel.JSTrim(query)
	if trimmed == "" || limit <= 0 || len(servers) == 0 {
		return "", false
	}
	return trimmed, true
}

// QueryFitsTheEmbedder reports whether a (trimmed) query is at most MaxPassageChars UTF-16 units;
// a longer one is searched by text only, without a warning.
func QueryFitsTheEmbedder(query string) bool { return UTF16Len(query) <= MaxPassageChars }
