package domain_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

// Every type of the domain that carries token material, a state or a verifier shows none of it,
// however it is held (secrettest.Holdings), through any verb, slog handler or JSON.
func TestSecretCarryingTypesAreUnprintableHoweverHeld(t *testing.T) {
	const access, refresh, state, verifier = "ZZ-sealed-access-0001", "ZZ-sealed-refresh-002", "ZZ-pending-state-0003", "ZZ-pkce-verifier-0004"
	forbidden := secrettest.Strings(t, access, refresh, state, verifier)
	tokens := domain.SealedTokens{Access: sealed(access), Refresh: sealed(refresh), ExpiresAt: &now}
	c := domain.ConnectOAuth(domain.OAuthConnect{UserID: "u1", Server: "gitea", IssuedFor: fp, Now: now, Tokens: tokens})
	pat := domain.PATConnect{UserID: "u1", Server: "yt", Token: sealed(access), IssuedFor: fp, Now: now}
	exp := now.Add(-1)
	plan := domain.Restore(domain.Snapshot{
		UserID: "u1", Server: "gitea", IssuedFor: fp, Kind: domain.KindOAuth, Status: domain.StatusReady,
		Access: sealed(access), Refresh: sealed(refresh), ExpiresAt: &exp,
	}).CredentialPlan(now, domain.PlanOptions{Config: fp})
	if plan.Action != domain.PlanRefresh {
		t.Fatalf("the plan must carry the refresh token: %s", plan.Action)
	}

	secrettest.AssertUnprintable(t, "Sealed", sealed(access), forbidden)
	secrettest.AssertUnprintable(t, "SealedTokens", tokens, forbidden)
	secrettest.AssertUnprintable(t, "Snapshot", c.Snapshot(), forbidden)
	secrettest.AssertUnprintable(t, "Connection", *c, forbidden)
	secrettest.AssertUnprintable(t, "OAuthConnect", domain.OAuthConnect{UserID: "u1", Tokens: tokens}, forbidden)
	secrettest.AssertUnprintable(t, "PATConnect", pat, forbidden)
	secrettest.AssertUnprintable(t, "CredentialPlan", plan, forbidden)
	secrettest.AssertUnprintable(t, "PendingConnect", domain.NewPendingConnect(kernel.NewSecret(state), "u1", "gitea", kernel.NewSecret(verifier), fp, now), forbidden)
	secrettest.AssertUnprintable(t, "PendingSnapshot", domain.PendingSnapshot{
		State: kernel.NewSecret(state), UserID: "u1", Server: "gitea", CodeVerifier: kernel.NewSecret(verifier), IssuedFor: fp, ExpiresAt: now,
	}, forbidden)
}
