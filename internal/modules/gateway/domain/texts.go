package domain

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

const (
	// MaxLoggedName bounds the server and tool names written to the call log, in UTF-16 units.
	MaxLoggedName = 200
	// MaxRejectionText bounds an upstream JSON-RPC error message in the agent's text, in UTF-16
	// units (499 and an ellipsis when longer).
	MaxRejectionText = 500

	mib = 1024 * 1024
)

// BoundName cuts a server or tool name for the call log to MaxLoggedName UTF-16 units (no
// ellipsis; half of a surrogate pair at the cut is dropped).
func BoundName(name string) string { return cutUTF16(name, MaxLoggedName) }

// Rejection is the code and message of an upstream JSON-RPC error answer. The message is the
// upstream's text: it goes to the agent only, never to a log.
type Rejection struct {
	Code    int
	Message string
}

// The agent-facing texts of a failed call_tool. name is the server's display name, publicUrl is
// PUBLIC_URL without trailing slashes.

// TextNotFound covers: the tool is not usable by this person now (ref is the agent's raw string).
func TextNotFound(ref string) string {
	return `Tool "` + ref + `" is not available. Use search_tools to find another.`
}

// TextInvalidArguments covers: path and message come from the argument validator.
func TextInvalidArguments(ref, path, message string) string {
	return "Invalid arguments at " + path + ": " + message + `. Call describe_tool("` + ref + `") for the schema.`
}

// TextNeedsConnection covers: the person has no connection to the server.
func TextNeedsConnection(name, publicURL string) string {
	return name + " is not connected. Ask the user to open " + publicURL + "/connections and connect " + name + "."
}

// TextExpired covers: the person's connection expired.
func TextExpired(name, publicURL string) string {
	return name + " connection expired. Ask the user to reconnect at " + publicURL + "/connections."
}

// TextRejected covers: the server's own credentials were rejected (also for auth kind none).
func TextRejected(name string) string {
	return name + " rejected the gateway credentials. Ask the administrator to check the server key."
}

// TextQuota covers: the daily limit is reached; resetAt prints as a JavaScript ISO date (UTC, with
// milliseconds).
func TextQuota(name string, resetAt time.Time) string {
	return "Daily limit for " + name + " reached. Resets at " + resetAt.UTC().Format("2006-01-02T15:04:05.000Z") + "."
}

// TextTimeout covers: the call did not finish in the server's timeout.
func TextTimeout(name string, timeoutMs int) string {
	return name + " did not respond in " + round2(float64(timeoutMs)/1000) + " s."
}

// TextTransport covers: the upstream (or the OAuth provider) cannot be reached; no detail.
func TextTransport(name string) string { return name + " is unavailable." }

// TextUpstreamRejected covers: upstream answered the call with a JSON-RPC error.
func TextUpstreamRejected(name string, r Rejection) string {
	message := kernel.JSTrim(r.Message)
	head := name + " rejected the call (JSON-RPC " + strconv.Itoa(r.Code) + ")"
	if message == "" {
		return head + "."
	}
	return head + ": " + cutRejection(message)
}

func cutRejection(text string) string {
	if utf16Len(text) > MaxRejectionText {
		return cutUTF16(text, MaxRejectionText-1) + "…"
	}
	return text
}

// TextTooLarge covers: the response exceeded the limit in bytes.
func TextTooLarge(name string, maxBytes int64) string {
	return name + " returned a response larger than " + round2(float64(maxBytes)/mib) + " MiB."
}

// round2 is Number(n.toFixed(2)) printed by JavaScript: the exact decimal value of the double
// is rounded to two places with an exact tie going UP (Go's strconv rounds ties to even), and
// trailing zeros are dropped. Only non-negative finite values occur; others print as "0".
func round2(x float64) string {
	if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
		return "0"
	}
	r := new(big.Rat).SetFloat64(x)
	r.Mul(r, big.NewRat(100, 1))
	r.Add(r, big.NewRat(1, 2))
	n := new(big.Int).Div(r.Num(), r.Denom()) // floor for a positive value
	q, m := new(big.Int).DivMod(n, big.NewInt(100), new(big.Int))
	out := q.String()
	if f := m.Int64(); f != 0 {
		frac := strconv.FormatInt(f+100, 10)[1:]
		out += "." + strings.TrimRight(frac, "0")
	}
	return out
}
