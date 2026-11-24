package app

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// ResealSummary is what a key rotation did to the server secrets. Failed lists the slugs left as
// they were: neither key opens the secret, or it kept changing under the rotation.
type ResealSummary struct {
	Resealed int
	Current  int
	Failed   []string
}

// resealAttempts: a compare-and-set lost to a concurrent write is retried this many times in all.
const resealAttempts = 3

// ResealServerSecrets is the catalog's part of a SECRETS_KEY rotation: every server secret sealed
// with from is sealed again with to. A secret already under to is left alone, so an interrupted
// run can simply be repeated. Each swap is a compare-and-set: a secret an admin replaced meanwhile
// is read again, not overwritten. No updated_at, no events. One server at a time; an error of
// the repository stops the run where it is. Plain secrets never leave this function.
func ResealServerSecrets(ctx context.Context, servers ServerRepository, from, to kernel.SecretBox) (ResealSummary, error) {
	summary := ResealSummary{Failed: []string{}}
	listing, err := servers.List(ctx)
	if err != nil {
		return summary, err
	}
	for _, l := range listing {
		sealed := l.Server.SealedSecret()
		slug := l.Server.Slug()
		for attempt := 1; sealed != nil; attempt++ {
			out := kernel.Reseal(sealed, from, to)
			if out.Kind == kernel.ResealCurrent {
				summary.Current++
				break
			}
			if out.Kind != kernel.ResealResealed {
				summary.Failed = append(summary.Failed, slug)
				break
			}
			replaced, err := servers.ReplaceSealedSecret(ctx, slug, sealed, out.Sealed.Reveal())
			if err != nil {
				return summary, err
			}
			if replaced {
				summary.Resealed++
				break
			}
			if attempt == resealAttempts {
				summary.Failed = append(summary.Failed, slug)
				break
			}
			again, err := servers.FindBySlug(ctx, slug)
			if err != nil {
				return summary, err
			}
			sealed = nil
			if again != nil {
				sealed = again.SealedSecret()
			}
		}
	}
	return summary, nil
}
