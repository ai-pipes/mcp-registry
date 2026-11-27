//go:build compat

package harness

import (
	"fmt"
	"os"
	"strings"
)

// Deviation names one behaviour of the reference image (1.0.x) that the Go server deliberately does NOT reproduce (an
// owner decision). A scenario that touches it has two expectations: the reference behaviour, asserted
// as before, and the decided behaviour, asserted when the stack reports Fixed(deviation).
//
// COMPAT_FIXED selects the decided behaviour: "all", or a comma-separated list of deviation ids, or
// empty for none. Unset means all: the default image under test is the image built from this tree,
// which has every decided behaviour. The reference (the published 1.0.1) runs with COMPAT_FIXED set
// and empty (make compat-reference-run), so every reference branch is asserted there. Adding a
// deviation needs the owner's decision.
type Deviation string

// The deviations. The description of each is in AllDeviations.
const (
	DevCreateUserKeepsRole  Deviation = "create-user-keeps-role"
	DevCreateServerSyncExit Deviation = "create-server-sync-exit"
	DevReadyBeforeServing   Deviation = "ready-before-serving"
	DevOAuthStateKept       Deviation = "oauth-state-kept"
	DevReindexWording       Deviation = "reindex-wording"
	DevRequestIDOnRefusals  Deviation = "request-id-on-refusals"
	DevConfigErrors         Deviation = "config-errors"
	DevAuditLoginVia        Deviation = "audit-login-via"
	DevRetryAfter429        Deviation = "token-429-retry-after"
	DevOIDCBindingKept      Deviation = "oidc-binding-kept"

	DevVectorKeptOnFailedReembedding Deviation = "vector-kept-on-failed-reembedding"
	DevStaleRemovalRechecksCatalog   Deviation = "stale-removal-rechecks-catalog"
)

// DeviationInfo is one registry entry.
type DeviationInfo struct {
	ID Deviation
	// Description is one line: the reference behaviour, then the decided behaviour.
	Description string
}

// AllDeviations lists every deviation; a test checks that each is used by a scenario.
var AllDeviations = []DeviationInfo{
	{DevCreateUserKeepsRole, "create-user without --admin demotes an existing admin -> an existing person's role changes only with an explicit flag (--admin promotes, --member demotes)"},
	{DevCreateServerSyncExit, "create-server exits 0 when the first sync fails -> exits 1, the server still exists, the output says the sync failed"},
	{DevReadyBeforeServing, "/healthz may answer 503 and search uses full text only until the embedding model has loaded -> after a start /healthz never answers 503 while the database is up, and the first search equals the settled one"},
	{DevOAuthStateKept, "a callback under another person's session (invalid_state) consumes the starter's pending state -> the refusal leaves the state usable"},
	{DevReindexWording, "reindex prints `reindexed <N> server(s)` for a document count -> `reindexed <N> tool(s)`"},
	{DevRequestIDOnRefusals, "the foreign-Host 403 on /mcp carries no X-Request-Id -> every response carries it, that refusal included"},
	{DevConfigErrors, "EMBEDDER=openai-compatible without EMBEDDER_URL/EMBEDDER_MODEL ends in an unhandled exception and OIDC_SCOPES without openid is refused without OIDC -> an ordinary `Invalid configuration:` error naming both variables, and OIDC_SCOPES is validated only when OIDC is configured"},
	{DevAuditLoginVia, "an OIDC sign-in writes auth.login with empty meta -> meta.via is \"oidc\""},
	{DevRetryAfter429, "the 429 for failed token checks on /mcp has no Retry-After -> it carries Retry-After with a positive integer"},
	{DevOIDCBindingKept, "any OIDC callback (a forged one included) clears the browser's mcp_login binding cookie before checking it, so a foreign callback breaks the sign-in in progress with invalid_state -> the cookie is cleared only when the callback's state matches it; a callback that does not match leaves it alone and the genuine sign-in completes"},
	{DevVectorKeptOnFailedReembedding, "when the embedder fails while a document that has a vector is re-indexed, the upsert writes embedding = NULL and the document loses vector search until a later sweep -> the text is updated, the stored vector stays and the row is marked (embed_failed_at) so the sweep re-embeds it after the one-hour back-off"},
	{DevStaleRemovalRechecksCatalog, "a catalog.tools_removed event deletes its listed refs without asking the catalog, so a late or replayed one deletes the live row of a tool that came back -> the handler removes only listed refs the catalog does not currently offer for indexing"},
}

// FixedEnv is the environment variable that selects the decided behaviours.
const FixedEnv = "COMPAT_FIXED"

// ParseFixed parses the value of COMPAT_FIXED: "" = none, "all" = every deviation, otherwise a
// comma-separated list of ids (spaces around ids are ignored, empty items are an error). An
// unknown id is an error that lists the valid ones.
func ParseFixed(value string) (map[Deviation]bool, error) {
	fixed := map[Deviation]bool{}
	value = strings.TrimSpace(value)
	if value == "" {
		return fixed, nil
	}
	if value == "all" {
		for _, d := range AllDeviations {
			fixed[d.ID] = true
		}
		return fixed, nil
	}
	valid := map[Deviation]bool{}
	ids := make([]string, 0, len(AllDeviations))
	for _, d := range AllDeviations {
		valid[d.ID] = true
		ids = append(ids, string(d.ID))
	}
	for _, item := range strings.Split(value, ",") {
		id := Deviation(strings.TrimSpace(item))
		if !valid[id] {
			return nil, fmt.Errorf("%s: unknown deviation %q; valid ids: all, %s", FixedEnv, string(id), strings.Join(ids, ", "))
		}
		fixed[id] = true
	}
	return fixed, nil
}

// fixedFromEnv reads COMPAT_FIXED; boot fails on an invalid value. Unset is "all" (the default image
// under test), set and empty is none (the reference).
func fixedFromEnv() (map[Deviation]bool, error) {
	value, set := os.LookupEnv(FixedEnv)
	if !set {
		value = "all"
	}
	return ParseFixed(value)
}

// Fixed reports whether the image under test is expected to show the decided behaviour of d
// instead of the reference one (COMPAT_FIXED is "all" or lists d). It is false for the reference image.
func (s *Stack) Fixed(d Deviation) bool { return s.fixed[d] }
