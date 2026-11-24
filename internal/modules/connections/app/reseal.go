package app

import (
	"context"
	"fmt"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

// ResealSummary is what a key rotation did to people's connections. Failed lists
// "<userID>/<server>" of connections left as they were because neither key opens one of their
// tokens (never nil).
type ResealSummary struct {
	Resealed int
	Current  int
	Failed   []string
}

type resealOutcome int

const (
	resealGone resealOutcome = iota
	resealFailed
	resealCurrent
	resealDone
)

// ResealConnections is the connections' part of a SECRETS_KEY rotation: the access, refresh and
// PAT tokens sealed with from are sealed again with to. Each connection is rewritten under its
// lock, one at a time, so a refresh running at the same time is never overwritten with older
// tokens, and status, expiry and updatedAt stay. A token already under `to` is left alone (an
// interrupted run can simply be repeated); a connection with a token neither key opens is left
// byte for byte and named in Failed; one deleted since the listing is skipped. An error from the
// store stops the run. No audit record: the caller records the rotation.
func ResealConnections(ctx context.Context, store ConnectionStore, from, to kernel.SecretBox) (ResealSummary, error) {
	summary := ResealSummary{Failed: []string{}}
	keys, err := store.ListKeys(ctx)
	if err != nil {
		return summary, err
	}
	for _, k := range keys {
		outcome, err := resealOne(ctx, store, from, to, k)
		if err != nil {
			return summary, err
		}
		switch outcome {
		case resealDone:
			summary.Resealed++
		case resealCurrent:
			summary.Current++
		case resealFailed:
			summary.Failed = append(summary.Failed, k.UserID+"/"+k.Server)
		case resealGone:
		}
	}
	return summary, nil
}

func resealOne(ctx context.Context, store ConnectionStore, from, to kernel.SecretBox, k ConnectionKey) (resealOutcome, error) {
	outcome := resealGone
	err := store.WithLock(ctx, k.UserID, k.Server, func(_ context.Context, c *domain.Connection) (*domain.Connection, error) {
		if c == nil {
			return nil, nil
		}
		access := c.AccessToken()
		refresh := c.RefreshToken()
		a := kernel.Reseal(access.Bytes(), from, to)
		parts := []kernel.ResealOutcome{a}
		var r kernel.ResealOutcome
		if !refresh.IsEmpty() {
			r = kernel.Reseal(refresh.Bytes(), from, to)
			parts = append(parts, r)
		}
		allCurrent := true
		for _, p := range parts {
			switch p.Kind {
			case kernel.ResealUnreadable:
				outcome = resealFailed
				return nil, nil
			case kernel.ResealResealed:
				allCurrent = false
			}
		}
		if allCurrent {
			outcome = resealCurrent
			return nil, nil
		}
		newAccess, newRefresh := access, refresh
		if a.Kind == kernel.ResealResealed {
			newAccess = domain.NewSealed(a.Sealed.Reveal())
		}
		if !refresh.IsEmpty() && r.Kind == kernel.ResealResealed {
			newRefresh = domain.NewSealed(r.Sealed.Reveal())
		}
		if err := c.ReplaceSealed(newAccess, newRefresh); err != nil {
			return nil, fmt.Errorf("connections: re-seal %s/%s: %w", k.UserID, k.Server, err)
		}
		outcome = resealDone
		return c, nil
	})
	return outcome, err
}
