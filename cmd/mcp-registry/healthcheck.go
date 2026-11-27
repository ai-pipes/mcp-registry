package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
)

// healthcheckTimeout bounds the whole probe; it stays below the image's HEALTHCHECK --timeout (5 s),
// so Docker always sees the command's own verdict.
const healthcheckTimeout = 4 * time.Second

// healthcheck is `mcp-registry healthcheck`, the image's HEALTHCHECK command (the image has no curl
// and no Node). It runs before the configuration is read and reads only PORT (config.Port, the
// server's rule): a container whose configuration is broken has no server answering, so it is
// reported unhealthy by the probe, never by the probe refusing to run. No key guard, no database.
// Exit 0 only when GET http://127.0.0.1:<PORT>/healthz answers 200; silent on success, one short
// line on stderr otherwise (never the response body).
func healthcheck(ctx context.Context, args []string, env map[string]string, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintln(stderr, "healthcheck: takes no arguments")
		return 2
	}
	port, err := config.Port(env)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "healthcheck: PORT must be an integer from 1 to 65535")
		return 1
	}
	return probe(ctx, port, healthcheckTimeout, stderr)
}

// probe GETs /healthz on the loopback address with the given limit: no proxy, no redirects.
func probe(ctx context.Context, port int, timeout time.Duration, stderr io.Writer) int {
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "healthcheck: cannot build the request")
		return 1
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "healthcheck: no answer from "+url)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "healthcheck: /healthz answered %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
