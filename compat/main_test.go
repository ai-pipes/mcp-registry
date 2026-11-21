//go:build compat

package compat_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }
