package app

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

// SummaryStatus is the state of a person's connection to a server as listed.
type SummaryStatus string

// The listed statuses; missing is "no row" (needs_connection).
const (
	SummaryReady   SummaryStatus = "ready"
	SummaryExpired SummaryStatus = "expired"
	SummaryMissing SummaryStatus = "missing"
)

// ConnectionSummary is one server of the person's list.
type ConnectionSummary struct {
	Server   string
	Name     string
	AuthKind catalog.AuthKind
	Status   SummaryStatus
}

// ListQuery asks for a person's connections.
type ListQuery struct{ Principal Principal }

// ListConnections is every active oauth_user and pat_user server the person may use, in the
// directory's (slug) order, with the state of their connection. A connection made for an older
// server configuration shows as expired (nothing is written: the row changes when it is used).
// Servers the person may not use, and connections to servers no longer listed, do not appear. The
// result is never nil.
func ListConnections(ctx context.Context, d Deps, q ListQuery) ([]ConnectionSummary, error) {
	servers, err := d.Servers.PersonalServersFor(ctx, q.Principal)
	if err != nil {
		return nil, err
	}
	connections, err := d.Connections.ListForUser(ctx, q.Principal.UserID)
	if err != nil {
		return nil, err
	}
	byServer := make(map[string]*domain.Connection, len(connections))
	for _, c := range connections {
		byServer[c.Server()] = c
	}
	out := make([]ConnectionSummary, 0, len(servers))
	for _, s := range servers {
		status := SummaryMissing
		if c, found := byServer[s.Slug]; found {
			status = SummaryStatus(c.StatusFor(fingerprint(s.Config)))
		}
		out = append(out, ConnectionSummary{Server: s.Slug, Name: s.Name, AuthKind: s.AuthKind, Status: status})
	}
	return out, nil
}
