package infra

import (
	"net/http"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

func newResponse(status int, header http.Header, body []byte) response {
	return response{status: status, header: header, body: kernel.NewSecretBytes(body)}
}
