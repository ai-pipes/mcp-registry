package domain_test

import (
	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func sealedBytes(b []byte) kernel.SecretBytes { return domain.SealedSecretOf(b) }
