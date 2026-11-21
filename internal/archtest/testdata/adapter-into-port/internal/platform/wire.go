package platform

import (
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/infra"
)

// Wire injects an infra adapter into an app function that takes an app interface.
func Wire() string { return app.Use(&infra.PgRepo{}) }
