package app

import (
	"context"
	"time"
)

const (
	// outageStreak is the number of consecutive failures after which the sweep checks whether the
	// embedder is down.
	outageStreak = 3
	// probeText is a known-good passage: it tells a poison streak from an embedder outage.
	probeText = "health probe"
	// DefaultSweepBudget is the wall-clock budget of one sweep, well under the outbox lease.
	DefaultSweepBudget = 30 * time.Second
)

// StopReason is why a sweep ended before its list was done.
type StopReason string

// The reasons a sweep stops early; the zero value is "ran to the end".
const (
	StoppedByOutage StopReason = "outage"
	StoppedByBudget StopReason = "budget"
)

// SweepOptions bound one sweep.
type SweepOptions struct {
	// Limit is the most documents taken (the sweep's batch).
	Limit int
	// Budget is the wall-clock budget; zero means DefaultSweepBudget.
	Budget time.Duration
}

// SweepResult is what a sweep did. Failed holds refs only, never content.
type SweepResult struct {
	Indexed      []string
	Failed       []string
	StoppedEarly StopReason
}

// IndexMissingEmbeddings is the recovery sweep: it re-indexes up to Limit documents stored
// without a vector, or marked after a failed re-embedding that kept their older vector (an
// embedder outage outlasted the outbox retries), in the index's order, one
// after the other. It does nothing until the embedder is ready, and logs nothing.
//
// A failure is an isolated poison document only if a later document succeeds; then it is marked
// failed (skipped for an hour). After outageStreak failures in a row a known-good passage is
// embedded: if that works the streak is poison (all marked, the sweep goes on), if it fails the
// embedder is down (the sweep stops, nothing marked). The sweep also stops when its time budget is
// spent. Documents not attempted are never marked. Errors of the index itself (reading the list,
// marking) are returned; one raised while marking after a success counts as that document's
// failure, as in the TypeScript code.
func IndexMissingEmbeddings(ctx context.Context, d Deps, opts SweepOptions) (SweepResult, error) {
	result := SweepResult{Indexed: []string{}, Failed: []string{}}
	if !d.Embedder.Ready(ctx) {
		return result, nil
	}
	budget := opts.Budget
	if budget == 0 {
		budget = DefaultSweepBudget
	}
	clock := d.clock()
	deadline := clock.Now().Add(budget)
	pending, err := d.Index.WithoutEmbedding(ctx, opts.Limit)
	if err != nil {
		return result, err
	}
	// Failures since the last success; marked only once a later document succeeds.
	var streak []string
	mark := func(refs []string) error {
		for _, ref := range refs {
			if err := d.Index.MarkEmbedFailed(ctx, ref); err != nil {
				return err
			}
		}
		return nil
	}
	for _, c := range pending {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if result.StoppedEarly != "" {
			break
		}
		if !clock.Now().Before(deadline) {
			result.StoppedEarly = StoppedByBudget
			break
		}
		err := IndexToolRefs(ctx, d, c.Server, []string{c.Ref})
		if err == nil {
			result.Indexed = append(result.Indexed, c.Ref)
			isolated := streak
			streak = nil
			err = mark(isolated)
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.Failed = append(result.Failed, c.Ref)
		streak = append(streak, c.Ref)
		if len(streak) < outageStreak {
			continue
		}
		if _, probeErr := d.Embedder.EmbedPassage(ctx, probeText); probeErr == nil {
			poison := streak
			streak = nil
			if err := mark(poison); err != nil {
				return result, err
			}
		} else {
			result.StoppedEarly = StoppedByOutage
		}
	}
	return result, nil
}
