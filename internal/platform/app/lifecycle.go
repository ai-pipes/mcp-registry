package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ai-pipes/mcp-registry/apps/server/migrations"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/migrate"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Operator-facing texts, copied from the reference v1.0.1:apps/server/src/platform/main.ts and bootstrap.ts.
const (
	msgKeyOldSet        = "SECRETS_KEY_OLD is set: key rotation in progress. Run `mcp-registry rotate-secrets-key`, then remove SECRETS_KEY_OLD and restart"
	msgKeyCreated       = "secrets key check created: SECRETS_KEY is now verified at every start"
	msgKeyNeedsRotation = "the secrets key check still opens with SECRETS_KEY_OLD only: run rotate-secrets-key"
	msgKeyMismatch      = `SECRETS_KEY does not match the data in this database; restore the original key (stored secrets cannot be read with this one). See "Recovering from a key mismatch" in the README.`
)

// msgNoOutboxWorker is new in the Go server (the TypeScript one always runs its worker).
const msgNoOutboxWorker = "outbox worker not started: no handler is registered"

func newStdoutLogger(level string) *slog.Logger { return logging.New(level, os.Stdout) }

// Run starts the server and blocks until it has stopped. ctx is cancelled to stop it (the entry
// point cancels it with an app.Signal cause on SIGTERM or SIGINT). The result is the process exit
// code: 0 after a clean stop, 1 when the start failed or the shutdown did not finish in time.
//
// A cancelled ctx interrupts the connection to the database and the wait for the migration lock
// (another server is migrating); once migrations are being applied and during the key check it
// does not (a migration is never cut off half way), and a signal that arrives meanwhile stops the
// start before the port is opened. A start stopped this way logs `shutting down` and `stopped`
// and returns 0.
func Run(ctx context.Context, o Options) int {
	logger := loggerOf(o)
	cfg := o.Config

	// First record of every start: a failed start shows which version failed.
	logger.Info("starting", "version", o.Version)
	if cfg.SecretsKeyOld.IsSet() {
		logger.Warn(msgKeyOldSet)
	}

	openCtx, cancelOpen := context.WithTimeout(ctx, connectTimeout)
	pool, err := db.Open(openCtx, cfg.DatabaseURL.Reveal(), databasePassword(cfg))
	cancelOpen()
	if err != nil {
		if ctx.Err() != nil {
			return stopBeforeServing(ctx, logger, nil, nil)
		}
		// db.Open's text is fixed and never quotes the address or a password.
		logging.Fatal(logger, err.Error())
		return 1
	}
	switch prepare(ctx, o, logger, pool) {
	case startRefused:
		pool.Close()
		return 1
	case startCancelled:
		return stopBeforeServing(ctx, logger, pool, nil)
	}

	if ctx.Err() != nil {
		// A signal arrived during the start: do not open the port, stop at once.
		return stopBeforeServing(ctx, logger, pool, nil)
	}

	rt, err := newRuntime(cfg, logger, pool, o.Version)
	if err != nil {
		logging.Fatal(logger, "cannot build the runtime", logging.ErrAttr(err))
		pool.Close()
		return 1
	}
	// The gateway's calls in flight are cancelled when the shutdown begins (and write their
	// transport_error row before the pool closes).
	callsCtx, cancelCalls := context.WithCancel(context.Background())
	defer cancelCalls()
	rt.CallsContext = callsCtx
	asm, err := assemble(modulesOf(o.modules, rt))
	if err != nil {
		// The text names an event type or a command, never data.
		logging.Fatal(logger, err.Error())
		pool.Close()
		return 1
	}
	// After the key check, before the first request: a module's one-time provisioning.
	runFirstStart(ctx, logger, asm.firstStart)
	if ctx.Err() == nil {
		// Slow preparation (the embedding model) is finished before the first request, so the
		// first search equals the settled one and /healthz never waits for it.
		runHooks(ctx, logger, asm.beforeServing, msgBeforeServingPanicked)
	}
	if ctx.Err() != nil {
		// A signal arrived while FirstStart or a BeforeServing hook ran: do not start the worker
		// or open the port.
		return stopBeforeServing(ctx, logger, pool, asm.closers)
	}
	stopWorker := startOutbox(logger, pool, asm.handlers, asm.sweep)
	stopScheduler := scheduler.Start(logger, asm.jobs)

	ln := o.listener
	if ln == nil {
		ln, err = net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(cfg.Port))
	}
	if err != nil {
		logging.Fatal(logger, "cannot listen on port "+strconv.Itoa(cfg.Port), logging.ErrAttr(err))
		limit, cancel := context.WithTimeout(context.Background(), shutdownLimitOf(o))
		defer cancel()
		_ = stopAllThenClose(limit, []func(context.Context) error{stopWorker, stopScheduler}, asm.closers)
		pool.Close()
		return 1
	}

	srv := &http.Server{
		Handler:           handler(cfg, logger, pool, webRootOf(o), asm.routes, asm.prefixes),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// `OPTIONS *` goes through the chain like every request instead of net/http's bare 200
		// without the security headers and request id.
		DisableGeneralOptionsHandler: true,
		// net/http's own messages stay JSON lines of the platform logger.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.Info("listening", "port", ln.Addr().(*net.TCPAddr).Port, "version", o.Version)

	exit := 0
	select {
	case <-ctx.Done():
		logger.Info("shutting down", "signal", signalName(ctx))
	case err := <-served:
		// Serve returns before Shutdown only when the listener broke.
		logger.Error("server stopped unexpectedly", logging.ErrAttr(err))
		exit = 1
	}

	return shutdown(o, logger, srv, pool, cancelCalls, []func(context.Context) error{stopWorker, stopScheduler}, asm.closers, exit)
}

// databasePassword is the PGPASSWORD_FILE value for db.Open, nil when unset.
func databasePassword(cfg config.Config) *string {
	if !cfg.DatabasePassword.IsSet() {
		return nil
	}
	pw := cfg.DatabasePassword.Reveal()
	return &pw
}

// The records logged when a module's FirstStart or BeforeServing panics; the panic value is never
// part of the record (it may quote values or secrets).
const (
	msgFirstStartPanicked    = "module first start panicked"
	msgBeforeServingPanicked = "module preparation before serving panicked"
)

// runFirstStart runs the modules' FirstStart functions in order. A panic is logged and the start
// goes on.
func runFirstStart(ctx context.Context, logger *slog.Logger, funcs []func(context.Context)) {
	runHooks(ctx, logger, funcs, msgFirstStartPanicked)
}

// runHooks runs funcs in order; a panic is logged as msg (error level, call frames) and the next
// one runs.
func runHooks(ctx context.Context, logger *slog.Logger, funcs []func(context.Context), msg string) {
	for _, f := range funcs {
		func() {
			defer func() {
				if recover() != nil {
					logger.Error(msg, "stack", logging.PanicFrames())
				}
			}()
			f(ctx)
		}()
	}
}

// startOutbox starts the delivery worker, but only when there is a handler. The worker marks an
// event of a type without a handler as processed (the TypeScript server registers every handler,
// so there it never happens); a Go image pointed at a 1.0.x database must therefore not run one
// before the handlers exist, or it would consume the pending catalog events for good. The merged
// sweep (nil when no module has one) runs from the worker's loop, so without handlers it does not
// run either. assemble has already checked that the table covers every emitted event type.
func startOutbox(logger *slog.Logger, pool *pgxpool.Pool, handlers map[string]outbox.Handler, sweep *outbox.Sweep) func(context.Context) error {
	if len(handlers) == 0 {
		logger.Info(msgNoOutboxWorker)
		return func(context.Context) error { return nil }
	}
	return outbox.StartWorker(outbox.WorkerOptions{
		Pool:     pool,
		Handlers: handlers,
		Interval: outboxInterval,
		Logger:   logger,
		Sweep:    sweep,
	})
}

func callsGraceOf(o Options) time.Duration {
	if o.callsGrace > 0 {
		return o.callsGrace
	}
	return callsGrace
}

func shutdownLimitOf(o Options) time.Duration {
	if o.shutdownTimeout > 0 {
		return o.shutdownTimeout
	}
	return shutdownLimit
}

// stopBeforeServing ends a start that was cancelled before the port was opened.
func stopBeforeServing(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, closers []func(context.Context) error) int {
	logger.Info("shutting down", "signal", signalName(ctx))
	if len(closers) > 0 {
		// Nothing else was started; release what the modules built (the embedder may be loading).
		limit, cancel := context.WithTimeout(context.Background(), shutdownLimit)
		if err := stopAllThenClose(limit, nil, closers); err != nil {
			logger.Error("shutdown failed", logging.ErrAttr(err))
		}
		cancel()
	}
	if pool != nil {
		pool.Close()
	}
	logger.Info("stopped")
	return 0
}

// startOutcome is the result of prepare.
type startOutcome int

const (
	startReady     startOutcome = iota
	startRefused                // the reason is logged
	startCancelled              // ctx was cancelled while waiting for the migration lock
)

// prepare applies the migrations and checks the secrets key. ctx cancels only the wait for the
// migration lock (migrate.Run does not let it interrupt the files it applies); the key check is
// not interrupted.
func prepare(ctx context.Context, o Options, logger *slog.Logger, pool *pgxpool.Pool) startOutcome {
	applied, err := migrate.Run(ctx, pool, migrations.FS)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return startCancelled
		}
		var newer *migrate.NewerDatabaseError
		var failed *migrate.MigrationError
		switch {
		case errors.As(err, &newer), errors.As(err, &failed):
			// Both texts are written for the operator and carry no data.
			logging.Fatal(logger, err.Error())
		default:
			logging.Fatal(logger, "migrations failed", logging.ErrAttr(err))
		}
		return startRefused
	}
	if applied == nil {
		applied = []string{}
	}
	logger.Info("migrations applied", "applied", applied)

	verdict, err := crypto.Verify(context.WithoutCancel(ctx), pool, crypto.NewKeys(o.Config.SecretsKey.Reveal(), o.Config.SecretsKeyOld.Reveal()))
	if err != nil {
		logging.Fatal(logger, "secrets key check failed", logging.ErrAttr(err))
		return startRefused
	}
	switch verdict {
	case crypto.KeyCheckCreated:
		logger.Info(msgKeyCreated)
	case crypto.KeyCheckNeedsRotation:
		logger.Warn(msgKeyNeedsRotation)
	case crypto.KeyCheckMismatch:
		logging.Fatal(logger, msgKeyMismatch)
		return startRefused
	}
	return startReady
}

// stopAll runs the stop functions at once and waits for all; the first error is returned.
func stopAll(ctx context.Context, stops ...func(context.Context) error) error {
	errs := make([]error, len(stops))
	var wg sync.WaitGroup
	for i, stop := range stops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = stop(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stopAllThenClose runs the stops at once and waits for all; only when every one returned without
// error the closers run, one after another, and every one is tried. The closers release what the
// stopped parts used (the embedder: a worker still indexing would get a closed embedder with a
// live context and clear the vectors it already has), so they never run beside a stop, and not at
// all when a stop failed or timed out (then something may still be using what they release; the
// process is about to exit). The errors are joined.
func stopAllThenClose(ctx context.Context, stops, closers []func(context.Context) error) error {
	if err := stopAll(ctx, stops...); err != nil {
		return err
	}
	var errs []error
	for _, closer := range closers {
		errs = append(errs, closer(ctx))
	}
	return errors.Join(errs...)
}

// shutdown stops accepting connections and drains requests while the worker and the scheduler
// stop, all under one limit (the gateway's calls in flight are cancelled after callsGrace); then the closers of the modules run, then the pool is closed, only
// after every one of them has returned (a request or a job in flight still uses it). exit is the
// code to return when it all goes well.
func shutdown(o Options, logger *slog.Logger, srv *http.Server, pool *pgxpool.Pool, cancelCalls func(), stops []func(context.Context) error, closers []func(context.Context) error, exit int) int {
	// The listeners close at once (srv.Shutdown, below); the gateway's calls in flight get
	// callsGrace to finish on their own, and the ones still running then are cancelled (an upstream
	// call blocked for minutes would otherwise hold the drain until the limit). Each cancelled call
	// still writes its log row, which needs the pool: the pool is closed below only after every
	// request has returned. The timer is stopped when the shutdown ends earlier, and a shutdown
	// with nothing in flight is not delayed: srv.Shutdown returns as soon as the server is idle.
	grace := time.AfterFunc(callsGraceOf(o), cancelCalls)
	defer grace.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownLimitOf(o))
	defer cancel()
	err := stopAllThenClose(ctx, append(stops, srv.Shutdown), closers)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Error("shutdown timed out")
		} else {
			logger.Error("shutdown failed", logging.ErrAttr(err))
		}
		// Drop what is still open; the pool is left for the process exit, since closing it waits
		// for the connections that a stuck request or job still holds.
		_ = srv.Close()
		return 1
	}
	pool.Close()
	logger.Info("stopped")
	return exit
}
