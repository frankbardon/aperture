package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// shutdownTimeout bounds how long serve waits for in-flight requests to drain on
// SIGINT/SIGTERM before forcing the listener closed.
const shutdownTimeout = 10 * time.Second

// serveCommand is `aperture serve`: it hand-wires the dependency graph
// (storage -> engine -> service -> HTTP server), boots a net/http server, and
// shuts it down gracefully on SIGINT/SIGTERM. This is the manual constructor-DI
// pattern Aperture mirrors from orbit's serve command — no DI framework.
func serveCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "serve",
		Usage: "Run the Aperture HTTP server",
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:  "addr",
				Usage: "TCP address to listen on",
				Value: ":8080",
			},
			&ucli.StringFlag{
				Name:  "seed",
				Usage: "path to a JSON/YAML seed model (defaults to the embedded example)",
			},
			&ucli.StringFlag{
				Name:  "store",
				Usage: "sqlite DSN for the backing store (defaults to in-memory)",
			},
		},
		Action: runServe,
	}
}

func runServe(ctx context.Context, cmd *ucli.Command) error {
	// Construct the dependency graph by hand: storage -> engine -> service ->
	// HTTP handler. Each layer is a plain constructor; there is no container.
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	handler := server.New(service.New(engine.New(store)))

	addr := cmd.String("addr")
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Trip ctx on the first SIGINT/SIGTERM so the select below can begin a
	// graceful shutdown.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		fmt.Fprintf(cmd.Writer, "aperture serving on %s\n", addr)
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return aerr.Wrap(aerr.APERTURE_BOOT, "cli: http server failed", err)
		}
		return nil
	case <-ctx.Done():
		fmt.Fprintln(cmd.Writer, "shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return aerr.Wrap(aerr.APERTURE_BOOT, "cli: graceful shutdown failed", err)
		}
		return nil
	}
}
