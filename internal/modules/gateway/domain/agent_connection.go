package domain

import "strings"

// PersonalStatus is the person's connection to a personal server as the connections module
// reports it; PersonalAbsent stands for "no entry for that server".
type PersonalStatus string

// The personal connection states.
const (
	PersonalReady   PersonalStatus = "ready"
	PersonalExpired PersonalStatus = "expired"
	PersonalMissing PersonalStatus = "missing"
	PersonalAbsent  PersonalStatus = ""
)

// AgentConnection is what the agent learns about a server's connection (list_servers,
// search_tools, describe_tool).
type AgentConnection string

// The connection states the agent sees.
const (
	ConnectionReady           AgentConnection = "ready"
	ConnectionNeedsConnection AgentConnection = "needs_connection"
	ConnectionExpired         AgentConnection = "expired"
	ConnectionNotRequired     AgentConnection = "not_required"
)

// AllAgentConnections lists the states in the enum order of the tools/list schema (a copy).
func AllAgentConnections() []AgentConnection {
	return []AgentConnection{ConnectionReady, ConnectionNeedsConnection, ConnectionExpired, ConnectionNotRequired}
}

// AgentConnectionOf covers: none and shared_secret servers need nothing from the person, whatever the
// status; personal servers are ready or expired as the connection says, anything else (missing,
// no entry) is needs_connection.
func AgentConnectionOf(kind AuthKind, status PersonalStatus) AgentConnection {
	if kind == AuthNone || kind == AuthSharedSecret {
		return ConnectionNotRequired
	}
	switch status {
	case PersonalReady:
		return ConnectionReady
	case PersonalExpired:
		return ConnectionExpired
	default:
		return ConnectionNeedsConnection
	}
}

// NeedsLink covers: the person has to act in the UI, so the agent gets the connections page link.
func (c AgentConnection) NeedsLink() bool {
	return c == ConnectionNeedsConnection || c == ConnectionExpired
}

// ConnectURL is the page where a person connects a server: PUBLIC_URL without trailing slashes,
// then /connections (list_servers' connectUrl).
func ConnectURL(publicURL string) string { return strings.TrimRight(publicURL, "/") + "/connections" }
