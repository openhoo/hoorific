package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"hoorific/internal/admin"
	"hoorific/internal/app"
	"hoorific/internal/console"
	"hoorific/internal/core"
	"hoorific/internal/credential"
	"hoorific/internal/gateway"
	"hoorific/internal/protocol"
	"hoorific/internal/store"
	"hoorific/internal/telemetry"
	"hoorific/internal/transport"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: hoorific serve|migrate|admin bootstrap|config validate|diff|apply|export --config FILE")
	}
	command := args[0]
	args = args[1:]
	sub := ""
	if command == "admin" || command == "config" {
		if len(args) == 0 {
			return errors.New("subcommand required")
		}
		sub = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configFile := flags.String("config", "", "strict JSON bootstrap configuration")
	tenant := flags.String("tenant", "default", "tenant identifier")
	subject := flags.String("subject", "bootstrap-owner", "initial owner identifier")
	file := flags.String("file", "", "runtime resource JSON input")
	tokenFile := flags.String("admin-token-file", "", "scoped administrative token file")
	revision := flags.Int64("expected-revision", 0, "expected configuration revision")
	prune := flags.Bool("prune", false, "delete omitted runtime resources")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *configFile == "" {
		return errors.New("--config is required")
	}
	f, err := os.Open(*configFile)
	if err != nil {
		return err
	}
	cfg, err := core.DecodeBootstrap(f)
	f.Close()
	if err != nil {
		return err
	}
	if command == "config" && sub == "validate" {
		fmt.Println("configuration valid")
		return nil
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	keys, err := credential.LoadKeyring(cfg.Encryption.KeyFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if command == "migrate" {
		if err = db.Migrate(ctx); err != nil {
			return err
		}
		fmt.Println("schema migrated")
		return nil
	}
	if err = db.CheckSchema(ctx); err != nil {
		return err
	}
	if command == "admin" && sub == "bootstrap" {
		code, e := db.CreateBootstrap(ctx, *tenant, *subject)
		if e != nil {
			return e
		}
		fmt.Println(code)
		return nil
	}
	if command == "config" {
		if *tokenFile == "" {
			return errors.New("config mutation/export requires --admin-token-file")
		}
		raw, e := os.ReadFile(*tokenFile)
		if e != nil {
			return e
		}
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(raw))))
		p, e := db.ResolveAdminToken(ctx, hex.EncodeToString(sum[:]))
		if e != nil {
			return errors.New("invalid administrative token")
		}
		var data json.RawMessage
		if sub == "apply" || sub == "diff" {
			if *file == "" {
				return errors.New("--file is required")
			}
			raw, e := os.ReadFile(*file)
			if e != nil {
				return e
			}
			data, e = json.Marshal(map[string]any{"expected_revision": *revision, "config": json.RawMessage(raw), "prune": *prune})
			if e != nil {
				return e
			}
		}
		if sub != "apply" && sub != "diff" && sub != "export" {
			return errors.New("unknown configuration action")
		}
		out, e := db.Execute(core.WithPrincipal(ctx, p), p, "config", "", sub, *revision, data)
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(append(out, '\n'))
		return e
	}
	if command != "serve" {
		return errors.New("unknown command")
	}
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()
	return serve(cfg, db, keys)
}
func serve(cfg core.BootstrapConfig, db *store.Store, keys credential.Keyring) error {
	telemetryRuntime, err := telemetry.Setup(context.Background(), cfg.Telemetry)
	if err != nil {
		return err
	}
	defer func() {
		if err := telemetryRuntime.Shutdown(context.Background()); err != nil {
			slog.Warn("telemetry shutdown incomplete", "error_type", fmt.Sprintf("%T", err))
		}
	}()
	if err := db.ValidateCredentials(context.Background(), keys); err != nil {
		return err
	}
	if err := db.ValidateRuntime(context.Background()); err != nil {
		return err
	}
	maintenanceCtx, maintenanceCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = db.RunMaintenance(maintenanceCtx)
	maintenanceCancel()
	if err != nil {
		return err
	}
	spool, err := transport.NewSpool(transport.SpoolConfig{Dir: filepath.Join(cfg.DataDir, "spool"), MaxTenantBytes: 2 << 30, MaxTotalBytes: 8 << 30})
	if err != nil {
		return err
	}
	if err := spool.CleanupOrphans(); err != nil {
		return err
	}
	credentials, err := credential.NewManager(keys, db)
	if err != nil {
		return err
	}
	pool, err := transport.NewPoolWithConfig(cfg.Transport)
	if err != nil {
		return err
	}
	defer pool.CloseIdleConnections()
	clients := app.ClientFactory(pool)
	runtimeState, err := app.NewRuntimeState(cfg, db)
	if err != nil {
		return err
	}
	defer runtimeState.Close()
	oauthFactory, deviceFactory := app.ConfiguredOAuthFactory(cfg, keys, db, clients)
	refreshStore, err := qualificationRefreshRepository(db)
	if err != nil {
		return err
	}
	refreshing, err := credential.NewRefreshingSource(credentials, refreshStore, oauthFactory)
	if err != nil {
		return err
	}
	identity := app.NewIdentitySource(refreshing)
	connectors := app.Builtins(clients, identity, cfg.SubscriptionConnectors.Enabled)
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	metrics := gateway.NewMetrics(registry)
	engine, err := gateway.New(gateway.Dependencies{Auth: db, Snapshots: runtimeState, Admission: db, Planner: db, Credentials: identity, Idempotency: db, Connectors: connectors, Codecs: protocol.Builtins(), Client: clients, Spool: spool, Resources: db, Keys: keys, Tickets: db, Metrics: metrics, Hints: runtimeState.Hints, PublicURL: cfg.PublicURLs["inference"]})
	if err != nil {
		return err
	}
	actions := app.NewActions(app.ActionDependencies{Store: db, Connectors: connectors, Credentials: identity, CredentialManager: credentials, Client: clients, SubscriptionEnabled: cfg.SubscriptionConnectors.Enabled, OAuthClientFactory: oauthFactory, DeviceCoordinatorFactory: deviceFactory})
	origin := cfg.PublicURLs["management"]
	if origin == "" {
		host := cfg.Listeners.Management
		if strings.HasPrefix(host, ":") {
			host = "127.0.0.1" + host
		}
		origin = "http://" + host
	}
	deps := admin.Dependencies{Repository: db, Auth: db, Actions: actions, Reconciler: actions, Playground: engine, PublicOrigin: origin, SecureCookies: strings.HasPrefix(origin, "https://"), Ready: func(ctx context.Context) error {
		if !engine.Ready() {
			return errors.New("gateway is draining")
		}
		return db.Ready(ctx)
	}, Metrics: promhttp.HandlerFor(registry, promhttp.HandlerOpts{})}
	if cfg.OIDC.Issuer != "" {
		secret := ""
		if cfg.OIDC.ClientSecretFile != "" {
			b, e := os.ReadFile(cfg.OIDC.ClientSecretFile)
			if e != nil {
				return e
			}
			secret = strings.TrimSpace(string(b))
		}
		deps.OIDC = &admin.OIDCConfig{Issuer: cfg.OIDC.Issuer, ClientID: cfg.OIDC.ClientID, ClientSecret: secret, RedirectURI: origin + "/admin/api/v1/auth/callback"}
	}
	management, err := admin.New(deps)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/admin/api/", management)
	mux.Handle("/health/", management)
	mux.Handle("/metrics", management)
	mux.Handle("/admin/", http.StripPrefix("/admin", console.New()))
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
	})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	requests, abort := context.WithCancel(context.Background())
	defer abort()
	infer := &http.Server{Addr: cfg.Listeners.Inference, Handler: telemetryRuntime.HTTPHandler("inference", engine), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return requests }}
	manage := &http.Server{Addr: cfg.Listeners.Management, Handler: telemetryRuntime.HTTPHandler("management", mux), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	li, err := net.Listen("tcp", infer.Addr)
	if err != nil {
		return err
	}
	defer li.Close()
	lm, err := net.Listen("tcp", manage.Addr)
	if err != nil {
		return err
	}
	defer lm.Close()
	errs := make(chan error, 6)
	go func() { errs <- runtimeState.Run(requests) }()
	go func() { errs <- engine.RunResourceReconciler(requests) }()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-requests.Done():
				return
			case <-ticker.C:
				passCtx, cancelPass := context.WithTimeout(requests, 10*time.Second)
				err := db.RunMaintenance(passCtx)
				cancelPass()
				if err != nil && requests.Err() == nil {
					slog.Warn("maintenance pass deferred", "error_type", fmt.Sprintf("%T", err))
				}
			}
		}
	}()
	go func() { errs <- infer.Serve(li) }()
	go func() { errs <- manage.Serve(lm) }()
	slog.Info("gateway ready", "inference", li.Addr().String(), "management", lm.Addr().String(), "mode", cfg.Mode)
	select {
	case <-ctx.Done():
	case e := <-errs:
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			abort()
			_ = infer.Close()
			_ = manage.Close()
			return e
		}
	}
	engine.Drain()
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- infer.Shutdown(shutdown) }()
	go func() { done <- manage.Shutdown(shutdown) }()
	for range 2 {
		if e := <-done; e != nil {
			abort()
			_ = infer.Close()
			_ = manage.Close()
		}
	}
	abort()
	if err := telemetryRuntime.Shutdown(shutdown); err != nil {
		slog.Warn("telemetry shutdown incomplete", "error_type", fmt.Sprintf("%T", err))
	}
	return nil
}

var _ io.Writer = os.Stdout
