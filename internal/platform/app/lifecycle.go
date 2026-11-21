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

// Operator-facing texts, copied from apps/server/src/platform/main.ts and bootstrap.ts.
const (
	msgKeyOldSet        = "SECRETS_KEY_OLD is set: key rotation in progress. Run `node dist/cli.js rotate-secrets-key`, then remove SECRETS_KEY_OLD and restart"
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
			return stopBeforeServing(ctx, logger, nil)
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
		return stopBeforeServing(ctx, logger, pool)
	}

	if ctx.Err() != nil {
		// A signal arrived during the start: do not open the port, stop at once.
		return stopBeforeServing(ctx, logger, pool)
	}

	rt, err := newRuntime(cfg, logger, pool, o.Version)
	if err != nil {
		logging.Fatal(logger, "cannot build the runtime", logging.ErrAttr(err))
		pool.Close()
		return 1
	}
	asm, err := assemble(modulesOf(o.modules, rt))
	if err != nil {
		// The text names an event type or a command, never data.
		logging.Fatal(logger, err.Error())
		pool.Close()
		return 1
	}
	// After the key check, before the first request: a module's one-time provisioning.
	runFirstStart(ctx, logger, asm.firstStart)
	stopWorker := startOutbox(logger, pool, asm.handlers)
	stopScheduler := scheduler.Start(logger, asm.jobs)

	ln := o.listener
	if ln == nil {
		ln, err = net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(cfg.Port))
	}
	if err != nil {
		logging.Fatal(logger, "cannot listen on port "+strconv.Itoa(cfg.Port), logging.ErrAttr(err))
		limit, cancel := context.WithTimeout(context.Background(), shutdownLimitOf(o))
		defer cancel()
		_ = stopAll(limit, stopWorker, stopScheduler)
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

	return shutdown(o, logger, srv, pool, []func(context.Context) error{stopWorker, stopScheduler}, exit)
}

// databasePassword is the PGPASSWORD_FILE value for db.Open, nil when unset.
func databasePassword(cfg config.Config) *string {
	if !cfg.DatabasePassword.IsSet() {
		return nil
	}
	pw := cfg.DatabasePassword.Reveal()
	return &pw
}

// msgFirstStartFailed is logged when a module's FirstStart fails; the error's text is never part
// of the record (it may quote values or secrets).
const msgFirstStartFailed = "module first start failed"

// runFirstStart runs the modules' FirstStart functions in order. A failure (or a panic) is logged
// and the start goes on.
func runFirstStart(ctx context.Context, logger *slog.Logger, funcs []func(context.Context) error) {
	for _, f := range funcs {
		func() {
			defer func() {
				if recover() != nil {
					logger.Error(msgFirstStartFailed, "stack", logging.PanicFrames())
				}
			}()
			if err := f(ctx); err != nil {
				logger.Error(msgFirstStartFailed, logging.ErrAttr(err))
			}
		}()
	}
}

// startOutbox starts the delivery worker, but only when there is a handler. The worker marks an
// event of a type without a handler as processed (the TypeScript server registers every handler,
// so there it never happens); a Go image pointed at a 1.0.x database must therefore not run one
// before the handlers exist, or it would consume the pending catalog events for good.
func startOutbox(logger *slog.Logger, pool *pgxpool.Pool, handlers map[string]outbox.Handler) func(context.Context) error {
	if len(handlers) == 0 {
		logger.Info(msgNoOutboxWorker)
		return func(context.Context) error { return nil }
	}
	return outbox.StartWorker(outbox.WorkerOptions{
		Pool:     pool,
		Handlers: handlers,
		Interval: outboxInterval,
		Logger:   logger,
	})
}

func shutdownLimitOf(o Options) time.Duration {
	if o.shutdownTimeout > 0 {
		return o.shutdownTimeout
	}
	return shutdownLimit
}

// stopBeforeServing ends a start that was cancelled before the port was opened.
func stopBeforeServing(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool) int {
	logger.Info("shutting down", "signal", signalName(ctx))
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

	verdict, err := crypto.Verify(context.WithoutCancel(ctx), pool, crypto.Keys{Current: o.Config.SecretsKey.Reveal(), Previous: o.Config.SecretsKeyOld.Reveal()})
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

// shutdown stops accepting connections and drains requests while the worker and the scheduler
// stop, all under one limit; then the pool is closed, only after every one of them has returned
// (a request or a job in flight still uses it). exit is the code to return when it all goes well.
func shutdown(o Options, logger *slog.Logger, srv *http.Server, pool *pgxpool.Pool, stops []func(context.Context) error, exit int) int {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownLimitOf(o))
	defer cancel()
	err := stopAll(ctx, append(stops, srv.Shutdown)...)
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
