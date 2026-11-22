package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/search"
	searchinfra "github.com/ai-pipes/mcp-registry/internal/modules/search/infra"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/infra/onnx"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
)

// The sweep that fills the vectors an embedder outage left empty: apps/server/src/platform/main.ts
// (EMBED_SWEEP_INTERVAL_MS, EMBED_SWEEP_LIMIT). It runs from the outbox worker's loop.
const (
	embedSweepEvery = 5 * time.Minute
	embedSweepLimit = 20
)

// The one record the search wiring adds (the TypeScript server logs nothing here: it loads the
// model on first use). A warning, not an error: text search keeps working and the embedder is
// tried again on use. "reason" is a fixed word, never a path or an error text.
const (
	msgEmbedderUnavailable = "search: local embedder unavailable, search is text-only until it loads"
	reasonNotBuilt         = "not_built"
	reasonLoadFailed       = "load_failed"
	reasonLoadTimeout      = "load_timeout"
)

// modelLoadBound is how long the start waits for the local model. A load that takes longer (a
// hung disk, a stuck mount) logs the same record with reason load_timeout and the start goes on;
// the load itself continues in the background and the embedder reports ready when it is done.
const modelLoadBound = 2 * time.Minute

// embedderHandle is the embedder the composition chose and what the lifecycle needs from it. load
// and close are nil for an embedder that has none (the OpenAI-compatible adapter holds no state).
type embedderHandle struct {
	search.Embedder
	// load blocks until the model is loaded and returns the load error.
	load func(ctx context.Context) error
	// close releases the model; it waits for a run in flight and is safe to call twice.
	close func() error
}

// embedderFor is the embedder named by EMBEDDER: `local` (the default) is the ONNX model read
// from MODEL_DIR, the library at the image's fixed path; `openai-compatible` is the HTTP adapter at
// EMBEDDER_URL. Building one loads nothing (the CLI builds every module for every command).
func embedderFor(cfg config.Config) embedderHandle {
	if cfg.Embedder == "openai-compatible" {
		return embedderHandle{Embedder: searchinfra.NewOpenAIEmbedder(cfg.EmbedderURL, cfg.EmbedderModel, nil)}
	}
	e := onnx.NewLocal(onnx.Options{ModelDir: cfg.ModelDir})
	return embedderHandle{Embedder: e, load: e.Load, close: e.Close}
}

// searchWiring is the search module built from the runtime. Building it makes no request and
// loads no model.
type searchWiring struct {
	rt       Runtime
	service  *search.Service
	embedder embedderHandle
	// loadBound is how long BeforeServing waits for the load (modelLoadBound; tests shorten it).
	loadBound time.Duration
}

func newSearchWiring(rt Runtime, cat catalogWiring) searchWiring {
	return searchWiringOver(rt, searchinfra.NewPgIndex(rt.Pool), embedderFor(rt.Config), cat.service)
}

// searchWiringOver builds the module over the given index, embedder and catalog (tests hand in an
// in-memory index and a fake embedder). It panics when one of them would be nil.
func searchWiringOver(rt Runtime, index search.Index, emb embedderHandle, tools search.ToolSource) searchWiring {
	return searchWiring{
		rt: rt, embedder: emb, loadBound: modelLoadBound,
		service: search.New(search.Deps{
			Index: index, Embedder: emb.Embedder, Tools: tools,
			Clock: rt.Clock, Logger: rt.Logger, MinSimilarity: rt.Config.SearchMinSimilarity,
		}),
	}
}

// module is what search adds to the server: handlers for ALL the catalog's event types (the
// assembly refuses a partial table, because the worker marks an event of an unhandled type as
// processed), the sweep, the model load before serving, the embedder's release and `reindex`.
// search_tools is the gateway's (a later milestone); search has no routes.
func (w searchWiring) module() Module {
	m := Module{
		Handlers: map[string]outbox.Handler{},
		Sweeps:   []outbox.Sweep{{Every: embedSweepEvery, Run: w.sweep}},
		Commands: []CLICommand{w.reindexCommand()},
	}
	for typ, h := range w.service.EventHandlers() {
		m.Handlers[typ] = outbox.Handler(h)
	}
	if w.embedder.load != nil {
		m.BeforeServing = w.loadModel
	}
	if w.embedder.close != nil {
		m.Closers = []func(context.Context) error{w.closeEmbedder}
	}
	return m
}

// sweep is indexMissingEmbeddings with the TypeScript period and limit; the result is dropped, as
// there. A cancelled run (the worker stops) returns an error wrapping context.Canceled.
func (w searchWiring) sweep(ctx context.Context) error {
	_, err := w.service.IndexMissingEmbeddings(ctx, search.SweepOptions{Limit: embedSweepLimit})
	return err
}

// closeEmbedder releases the embedder, but gives up waiting when ctx ends: Close waits for a load
// or a run in flight, and a hung one must not hold the shutdown past its limit. The shutdown then
// logs its timeout and exits 1; the close goes on in its goroutine until the process ends.
func (w searchWiring) closeEmbedder(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- w.embedder.close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loadModel loads the local embedder before the server accepts requests (decided deviation
// ready-before-serving: the first search is answered with the vector half, and /healthz never
// reports the model). A failure never stops the start: one warning and text search; the embedder
// tries again on its next use, so a model that appears later is picked up. The wait is bounded
// (loadBound): past it the same warning with reason load_timeout, and the start goes on while the
// load finishes in the background. A load cut short by the shutdown logs nothing.
func (w searchWiring) loadModel(ctx context.Context) {
	done := make(chan error, 1)
	go func() { done <- w.embedder.load(ctx) }()
	timer := time.NewTimer(w.loadBound)
	defer timer.Stop()
	var err error
	reason := reasonLoadFailed
	select {
	case err = <-done:
	case <-timer.C:
		w.rt.Logger.Warn(msgEmbedderUnavailable, "reason", reasonLoadTimeout)
		return
	case <-ctx.Done():
		return
	}
	if err == nil || ctx.Err() != nil {
		return
	}
	if errors.Is(err, onnx.ErrNotBuilt) {
		reason = reasonNotBuilt
	}
	w.rt.Logger.Warn(msgEmbedderUnavailable, "reason", reason, logging.ErrAttr(err))
}

// reindexCommand is `reindex` (cli-commands.ts reindexCommand; the guard only warns).
func (w searchWiring) reindexCommand() CLICommand {
	return CLICommand{Name: "reindex", Usage: `  reindex
      Rebuild the search index for every server.
`, Guard: GuardWarn, Run: w.reindex}
}

// reindex rebuilds the index from the catalog, drops the documents of servers that are gone and
// prints `reindexed <n> tool(s)`, n being the rows of the index afterwards (decided deviation
// reindex-wording: the reference says `server(s)` for the same number). An embedder failure does
// not stop the run: every tool gets its text row first, then the command fails with
// `error: unexpected failure` (the reference ends with an unhandled rejection), exit 1, and what
// was written stays. The process loads the model itself, on its first embedding, and releases it
// on exit.
func (w searchWiring) reindex(ctx context.Context, cio CLIIO, args []string) error {
	p, err := argSpec{}.parse(args)
	if err != nil {
		return err
	}
	if err := p.expectPositionals(0); err != nil {
		return err
	}
	count, err := w.service.ReindexAll(ctx)
	if err != nil {
		if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return CommandError(msgInterrupted)
		}
		return err
	}
	out(cio, fmt.Sprintf("reindexed %d tool(s)", count))
	return nil
}
