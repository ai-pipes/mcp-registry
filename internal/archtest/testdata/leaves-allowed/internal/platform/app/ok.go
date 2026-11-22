// Fixture: leaves-allowed
package app

import (
	_ "github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	_ "github.com/ai-pipes/mcp-registry/internal/modules/catalog/infra"
	_ "github.com/ai-pipes/mcp-registry/internal/platform/db"
	_ "github.com/ai-pipes/mcp-registry/internal/platform/config"
	_ "github.com/ai-pipes/mcp-registry/internal/platform/migrate"
	_ "github.com/ai-pipes/mcp-registry/internal/platform/upstream"
)
