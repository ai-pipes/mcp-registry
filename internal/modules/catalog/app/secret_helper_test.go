package app_test

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
)

// getServerSecret is the opened server secret, unbound to any configuration. It is a test helper
// on purpose: no production code may read a secret without naming the configuration it is for
// (the public API has GetServerSecretFor). ok is false when there is none (unknown server or no
// secret). A secret no key opens is an error.
func getServerSecret(ctx context.Context, deps app.Deps, slug string) (secret string, ok bool, err error) {
	server, err := deps.Servers.FindBySlug(ctx, slug)
	if err != nil || server == nil || server.SealedSecret() == nil {
		return "", false, err
	}
	plain, err := deps.Box.Open(server.SealedSecret())
	if err != nil {
		return "", false, err
	}
	return plain, true, nil
}
