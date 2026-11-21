package app

import (
	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// Repo is a port declared by app.
type Repo interface{ Get() string }

// Use takes a port.
func Use(r Repo) string { return r.Get() }

// Fail passes a domain error into a kernel function that takes error.
func Fail() string { return kernel.Label(&domain.NotFound{}) }
