// Package catalog is the catalog module: servers (the upstream MCP servers people may use) and
// the snapshots of their tools. This root package is the module's public surface, the only one
// other modules and the composition import: the types they exchange (as aliases of the module's
// own, so they never name an internal package), the event types and payloads the search module
// listens to, and Service, the queries and commands the gateway, connections, search and the CLI
// call.
//
// The REST routes are in catalog/http (Routes) and the Postgres repositories in catalog/infra
// (NewPg); this package imports neither, as a module's root never imports its adapters. The
// composition builds Deps from the repositories and the platform and hands it to New and to
// http.Routes.
package catalog
