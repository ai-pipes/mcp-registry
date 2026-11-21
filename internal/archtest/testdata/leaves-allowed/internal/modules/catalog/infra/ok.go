// Fixture: leaves-allowed
package infra

import (
	_ "github.com/ai-pipes/mcp-registry/internal/kernel"
	_ "github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	_ "github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	_ "github.com/ai-pipes/mcp-registry/internal/platform/db"
	_ "github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	_ "github.com/ai-pipes/mcp-registry/internal/platform/logging"
)
