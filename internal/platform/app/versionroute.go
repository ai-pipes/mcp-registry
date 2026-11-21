package app

import (
	"encoding/json"
	"net/http"
)

// versionHandler is `GET /api/version` (version-route.ts): the gateway version, for any signed-in
// person. The session guard is the prefix middleware of the platform module.
func versionHandler(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(struct {
			Version string `json:"version"`
		}{version})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}
