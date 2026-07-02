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

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/delegation"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/impersonation"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/rules"
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
			&ucli.StringFlag{
				Name:    "auth",
				Usage:   "authenticator adapter: dev|oidc|parsec (overrides APERTURE_AUTH_MODE; defaults to dev — bearer is the principal id, no external IdP)",
				Sources: ucli.EnvVars(auth.EnvMode),
			},
			&ucli.BoolFlag{
				Name:    "enforce-membership",
				Usage:   "deny any decision whose principal is not a member of the active account, before grants are consulted (defence-in-depth; lets shared roles be reused across accounts safely)",
				Sources: ucli.EnvVars("APERTURE_ENFORCE_MEMBERSHIP"),
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

	// Construct the authenticator from configuration (env + the --auth flag), then
	// apply it as request middleware so HTTP requests resolve to an Aperture
	// principal. The default adapter is dev/static (bearer == principal id), so
	// Aperture runs with NO external IdP; oidc and parsec are opt-in via config.
	authCfg := auth.ConfigFromEnv()
	if mode := cmd.String("auth"); mode != "" {
		authCfg.Mode = auth.Mode(mode)
	}
	authn, err := authCfg.Build(ctx)
	if err != nil {
		return aerr.Wrap(aerr.APERTURE_BOOT, "cli: building the authenticator failed", err)
	}

	// Build the fully-wired facade so HTTP, Twirp, and CLI drive ONE mutation
	// path: the engine for decisions + authority, the admin gate for tier checks,
	// and the delegation / impersonation services for their own gated mutations.
	//
	// Wire the rules engine (E2-S3) over a storage-backed rule source so
	// rule-backed scope strategies (E2-S1) resolve the SAME rules the node editor
	// (E7) saves through PutRule — a saved rule takes effect on the next decision
	// with no second rule store. Scope resolution falls back to literal pattern
	// matching for grants with no strategy, so E1 behaviour is preserved. The
	// storage source is also handed to the facade (WithRuleSource) so the editor's
	// live what-if can preview an UNSAVED rule read-only.
	// Build the object-metadata providers declared in the seed's `providers:`
	// section (E-provider): each entry links an object-type to a real data source
	// (a CSV file today, a database later) with no Go wiring. The same *Registry
	// feeds BOTH the rules engine's metadata fetcher (so a rule can read
	// object.category_id) AND the scope resolver's object lister (so implicit /
	// exclusive scopes can enumerate a type's objects). When no providers are
	// declared, both stay nil and the server behaves exactly as before.
	providerDoc, err := seedDocument(cmd.String("seed"))
	if err != nil {
		return err
	}
	reg, err := providerDoc.BuildRegistry(seedBaseDir(cmd.String("seed")))
	if err != nil {
		return aerr.Wrap(aerr.APERTURE_BOOT, "cli: building object providers failed", err)
	}
	var fetcher rules.MetadataFetcher // nil => empty object metadata (unchanged default)
	scopeDeps := engine.ScopeDeps{}
	if len(providerDoc.Providers) > 0 {
		fetcher = lenientFetcher{reg: reg}
		scopeDeps.Lister = reg
	}

	ruleSource := service.NewStorageRuleSource(store)
	ruleEngine := rules.NewEngine(ruleSource, fetcher)
	scopeDeps.Rules = ruleEngine
	engOpts := []engine.Option{engine.WithScopeResolution(nil, scopeDeps)}
	if cmd.Bool("enforce-membership") {
		// Defence-in-depth: a non-member of the active account is denied before any
		// grant is read, which is what lets a single shared role (manager,
		// analyst, ...) be reused across customer accounts without one customer's
		// account-scoped grants leaking to another customer's members.
		engOpts = append(engOpts, engine.WithMembershipEnforcement())
	}
	eng := engine.New(store, engOpts...)

	// Wire the append-only audit trail (E4-S2) through the same store so the
	// mutation/impersonation/delegation record is durable and the E6-S4 audit
	// viewer has data to query. Mutations are always recorded; decisions are
	// sampled — sample every decision here so the demo trail is legible. The
	// recorder owns a background writer that Close flushes on shutdown.
	rec := audit.New(store, audit.WithSampleRate(1))
	defer func() { _ = rec.Close() }()

	svc := service.New(eng,
		service.WithStorage(store),
		service.WithGate(authz.NewGate(eng)),
		service.WithDelegation(delegation.New(store, eng)),
		service.WithImpersonation(impersonation.New(store, eng)),
		service.WithAudit(rec),
		service.WithRuleSource(ruleSource, fetcher),
		service.WithProviders(reg),
	)

	handler := server.Authenticate(authn, server.New(svc))

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
