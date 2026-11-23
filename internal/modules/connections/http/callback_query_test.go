package http_test

import (
	"net/http/httptest"
	"testing"
)

// The callback reads its query as the reference (Hono) does: a provider's code with a raw ; or a
// stray % connects, instead of being refused as denied.
func TestCallbackReadsItsQueryLikeTheReference(t *testing.T) {
	for _, c := range []struct{ name, code, want string }{
		{"a raw semicolon", "a;b", "a;b"},
		{"a plus is a space", "a+b", "a b"},
		{"an escaped plus", "a%2Bb", "a+b"},
		{"an invalid escape stays", "ab%ZZ", "ab%ZZ"},
		{"invalid UTF-8 stays as written", "%FF", "%FF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			state := e.start(t, "alice", "gitea")
			if loc := e.callback(t, "alice", "code="+c.code+"&state="+state); loc != "/connections?connected=gitea" {
				t.Fatalf("Location %q", loc)
			}
			if len(e.oauth.codes) != 1 || e.oauth.codes[0] != c.want {
				t.Fatalf("the provider got %q, want %q", e.oauth.codes, c.want)
			}
		})
	}
	t.Run("the first occurrence of a key wins", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		if loc := e.callback(t, "alice", "code=first&state="+state+"&code=second"); loc != "/connections?connected=gitea" || e.oauth.codes[0] != "first" {
			t.Fatalf("Location %q codes %q", loc, e.oauth.codes)
		}
	})
	t.Run("a NUL in the state is invalid_state, a state with a semicolon is just unknown", func(t *testing.T) {
		e := newEnv(t)
		if loc := e.callback(t, "alice", "code=x&state=%00"); loc != "/connections?error=invalid_state" {
			t.Fatal(loc)
		}
		if loc := e.callback(t, "alice", "code=x&state=a;b"); loc != "/connections?error=invalid_state" {
			t.Fatal(loc)
		}
	})
	t.Run("raw bytes that are not UTF-8 in the state: invalid_state, never the database", func(t *testing.T) {
		e := newEnv(t)
		// The reference's URL parser turns raw bytes into text, so the state is unknown there; here
		// Postgres would refuse such a text (SQLSTATE 22021) and the person would see "internal".
		for _, state := range []string{"\xff\xfe", "ab\xc3", "\x80"} {
			if loc := e.callback(t, "alice", "code=x&state="+state); loc != "/connections?error=invalid_state" {
				t.Fatalf("state %q: %s", state, loc)
			}
		}
		if len(e.oauth.codes) != 0 {
			t.Fatal("the provider was reached")
		}
	})
}

// A client that goes away after the provider redirected it must not leave a consumed state without
// a stored connection: the use case runs detached from the request.
func TestCallbackSurvivesAClientThatWentAway(t *testing.T) {
	e := newEnv(t)
	state := e.start(t, "alice", "gitea")
	ctx, cancel := contextWithCancel()
	cancel()
	req := httptest.NewRequest("GET", "/api/connections/callback?code=C1&state="+state, nil).WithContext(ctx)
	req.Header.Set("Cookie", "sid=alice")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); loc != "/connections?connected=gitea" {
		t.Fatalf("Location %q", loc)
	}
	if !e.store.has("alice", "gitea") || e.store.cancelledWrites != 0 || e.pending.size() != 0 {
		t.Fatalf("stored=%v cancelled writes=%d pending=%d", e.store.has("alice", "gitea"), e.store.cancelledWrites, e.pending.size())
	}
}

// The routes go through the service's completed dependencies: the one single flight of the process.
func TestServiceDepsCarryTheOneSingleFlight(t *testing.T) {
	e := newEnv(t)
	e.deps.Flights = nil
	svc := e.service(t)
	d := svc.Deps()
	if d.Flights == nil || d.Flights != svc.Deps().Flights {
		t.Fatal("the service's deps have no single flight, or a new one for every call")
	}
	if d.Connections == nil || d.RedirectURI != redirectURI {
		t.Fatalf("incomplete deps: %+v", d)
	}
}
