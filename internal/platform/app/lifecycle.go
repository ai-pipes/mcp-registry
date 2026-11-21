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

func newStdoutLogger(level string) *slog.Logger { return logging.New(level, os.Stdout) }

// Run starts the server and blocks until it has stopped. ctx is cancelled to stop it (the entry
// point cancels it with an app.Signal cause on SIGTERM or SIGINT). The result is the process exit
// code: 0 after a clean stop, 1 when the start failed or the shutdown did not finish in time.
//
// The migrations and the key check are not interrupted by ctx, so a migration is never cut off
// half way; a signal that arrives meanwhile stops the start before the port is opened.
func Run(ctx context.Context, o Options) int {
	logger := loggerOf(o)
	cfg := o.Config

	// First record of every start: a failed start shows which version failed.
	logger.Info("starting", "version", o.Version)
	if len(cfg.SecretsKeyOld) > 0 {
		logger.Warn(msgKeyOldSet)
	}

	startCtx := context.WithoutCancel(ctx)
	openCtx, cancelOpen := context.WithTimeout(startCtx, connectTimeout)
	pool, err := db.Open(openCtx, cfg.DatabaseURL.Reveal(), databasePassword(cfg))
	cancelOpen()
	if err != nil {
		// db.Open's text is fixed and never quotes the address or a password.
		logging.Fatal(logger, err.Error())
		return 1
	}
	if !prepare(startCtx, o, logger, pool) {
		pool.Close()
		return 1
	}

	if ctx.Err() != nil {
		// A signal arrived during the start: do not open the port, stop at once.
		logger.Info("shutting down", "signal", signalName(ctx))
		pool.Close()
		logger.Info("stopped")
		return 0
	}

	stopWorker := outbox.StartWorker(outbox.WorkerOptions{
		Pool:     pool,
		Handlers: map[string]outbox.Handler{},
		Interval: outboxInterval,
		Logger:   logger,
	})
	stopScheduler := scheduler.Start(logger, nil)

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
		Handler:           handler(cfg, logger, pool, webRootOf(o), o.mount),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
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
	if cfg.DatabasePassword == nil {
		return nil
	}
	pw := cfg.DatabasePassword.Reveal()
	return &pw
}

func shutdownLimitOf(o Options) time.Duration {
	if o.shutdownTimeout > 0 {
		return o.shutdownTimeout
	}
	return shutdownLimit
}

// prepare applies the migrations and checks the secrets key; false means the start is refused
// (the reason is logged).
func prepare(ctx context.Context, o Options, logger *slog.Logger, pool *pgxpool.Pool) bool {
	applied, err := migrate.Run(ctx, pool, migrations.FS)
	if err != nil {
		var newer *migrate.NewerDatabaseError
		var failed *migrate.MigrationError
		switch {
		case errors.As(err, &newer), errors.As(err, &failed):
			// Both texts are written for the operator and carry no data.
			logging.Fatal(logger, err.Error())
		default:
			logging.Fatal(logger, "migrations failed", logging.ErrAttr(err))
		}
		return false
	}
	if applied == nil {
		applied = []string{}
	}
	logger.Info("migrations applied", "applied", applied)

	verdict, err := crypto.Verify(ctx, pool, crypto.Keys{Current: o.Config.SecretsKey.Reveal(), Previous: o.Config.SecretsKeyOld.Reveal()})
	if err != nil {
		logging.Fatal(logger, "secrets key check failed", logging.ErrAttr(err))
		return false
	}
	switch verdict {
	case crypto.KeyCheckCreated:
		logger.Info(msgKeyCreated)
	case crypto.KeyCheckNeedsRotation:
		logger.Warn(msgKeyNeedsRotation)
	case crypto.KeyCheckMismatch:
		logging.Fatal(logger, msgKeyMismatch)
		return false
	}
	return true
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
