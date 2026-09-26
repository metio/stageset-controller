// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// The stageset-controller manager: registers the stages.metio.wtf/v1 scheme
// and starts the StageSet reconciler.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	// Embed the IANA time zone database: update-window timeZones resolve via
	// time.LoadLocation, and the distroless static runtime image ships no
	// /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/cliflags"
	"github.com/metio/stageset-controller/internal/controller"
	"github.com/metio/stageset-controller/internal/gate"
	"github.com/metio/stageset-controller/internal/imageverify/sigstore"
	"github.com/metio/stageset-controller/internal/mcp"
	"github.com/metio/stageset-controller/internal/metrics"
	"github.com/metio/stageset-controller/internal/observability"
	"github.com/metio/stageset-controller/internal/opstate"
	"github.com/metio/stageset-controller/internal/rollbackstore"
	"github.com/metio/stageset-controller/internal/startupcheck"
	"github.com/metio/stageset-controller/internal/webhook/selfsigned"
)

var scheme = runtime.NewScheme()

// Stamped at build time via -ldflags="-X main.version=… -X main.commit=…".
var (
	version = "development"
	commit  = "unknown"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(stagesv1.AddToScheme(scheme))

	// Register ExternalArtifact (source.toolkit.fluxcd.io/v1) as Unstructured so
	// the cached client can list/get/watch it without a typed dependency on
	// source-controller's API.
	eaGVK := artifact.ExternalArtifactGVK
	scheme.AddKnownTypeWithName(eaGVK, &unstructured.Unstructured{})
	listGVK := eaGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
}

func main() {
	// SetupSignalHandler installs the SIGINT/SIGTERM handler exactly once per
	// process; keeping it in main (not run) lets run be called repeatedly from
	// tests with an ordinary context.
	ctx := ctrl.SetupSignalHandler()
	os.Exit(run(ctx, os.Args[1:], os.Environ(), os.Stderr))
}

// run is the testable seam under main: it takes its process-affecting inputs
// (the manager context, args, env, stderr) as parameters and returns a Unix
// exit code — 0 success, 1 runtime/validation failure, 2 flag parse error — so
// flag validation can be exercised in tests without contacting an apiserver.
// A fresh FlagSet per call means two invocations in one process don't panic
// with "flag redefined".
func run(ctx context.Context, args, env []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("stageset-controller", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := cliflags.Register(fs)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if err := c.Validate(); err != nil {
		fmt.Fprintln(stderr, "stageset-controller:", err)
		return 2
	}

	logger := observability.NewLogger(stderr, *c.LogLevel, *c.LogFormat)
	slog.SetDefault(logger)
	// Route controller-runtime's own logs (leader election, cache, manager,
	// internal reconcile) through the same slog handler so they share the
	// configured JSON/text format and level.
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))
	setupLog := logger.With("logger", "setup")
	setupLog.Info("starting stageset-controller", "version", version, "commit", commit)

	tracingShutdown, err := observability.InitTracer(ctx, observability.TracingConfig{
		Endpoint:       *c.TracingEndpoint,
		Insecure:       *c.TracingInsecure,
		ServiceVersion: version,
		SampleRatio:    *c.TracingSampleRatio,
	})
	if err != nil {
		setupLog.Error("unable to init tracer", "error", err)
		return 1
	}
	defer func() {
		// Bounded shutdown so a slow collector doesn't hang the
		// process — five seconds is generous for a flush.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracingShutdown(shutdownCtx)
	}()

	// Validate and construct the rollback store before any cluster contact, so a
	// flag mistake (both backends set, or an incomplete S3 config) fails fast
	// with a clear message rather than silently leaving rollback disabled or
	// erroring only after the manager has connected.
	rollbackStore, err := buildRollbackStore(c)
	if err != nil {
		setupLog.Error("invalid rollback store configuration", "error", err)
		return 1
	}
	switch {
	case *c.RBPath != "":
		setupLog.Info("rollback store enabled", "backend", "filesystem", "path", *c.RBPath)
		// The file store persists rendered output — including Secret data — to
		// this directory. Unlike the S3 backend it cannot set encryption at rest
		// itself, so the volume must provide it.
		setupLog.Info("ensure the rollback-store volume is encrypted at rest (encrypted StorageClass / LUKS / cloud-disk encryption); the file store writes rendered Secret data in the clear", "path", *c.RBPath)
	case *c.RBS3Endpoint != "":
		setupLog.Info("rollback store enabled", "backend", "s3", "endpoint", *c.RBS3Endpoint, "bucket", *c.RBS3Bucket)
	}

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error("unable to load kubeconfig", "error", err)
		return 1
	}

	// The manager's availability is shared state with three readers: the
	// supervisor writes it, the probe endpoints report it, and a gauge alerts on
	// it.
	state := opstate.New()
	metrics.SetManagerState(state)

	// The probe and metrics endpoints are bound here rather than by the manager.
	// A manager that cannot be built takes its own servers with it, and that is
	// exactly when the readiness probe and the availability gauge carry
	// information; a rebuilt manager would also have to re-bind the same ports.
	probeServer, err := serveProbes(*c.ProbeAddr, state, logger)
	if err != nil {
		setupLog.Error("unable to bind the probe endpoint", "error", err, "addr", *c.ProbeAddr)
		return 1
	}
	metricsServer, err := serveMetrics(*c.MetricsAddr, logger)
	if err != nil {
		shutdownServer(probeServer)
		setupLog.Error("unable to bind the metrics endpoint", "error", err, "addr", *c.MetricsAddr)
		return 1
	}
	defer func() {
		shutdownServer(metricsServer)
		shutdownServer(probeServer)
	}()

	var webhookRenewerDone <-chan struct{}
	if *c.EnableWebhook {
		switch *c.WebhookCertMode {
		case "cert-manager":
			// External tooling provisions tls.crt/tls.key under the cert dir.
		case "self-signed":
			if *c.WebhookVWCName == "" {
				setupLog.Error("--webhook-validating-config-name is required for --webhook-cert-mode=self-signed", "error", errors.New("missing flag"))
				return 1
			}
			ns := *c.WebhookServiceNS
			if ns == "" {
				ns = inClusterNamespace()
			}
			done, serr := provisionSelfSignedWebhookCert(ctx, logger, restCfg, selfsigned.Input{
				ServiceName: *c.WebhookServiceName,
				Namespace:   ns,
				Validity:    *c.WebhookCertValidity,
			}, *c.WebhookCertDir, *c.WebhookVWCName)
			if serr != nil {
				setupLog.Error("unable to provision self-signed webhook cert", "error", serr)
				return 1
			}
			webhookRenewerDone = done
			setupLog.Info("self-signed webhook cert provisioned", "certDir", *c.WebhookCertDir, "vwc", *c.WebhookVWCName)
		default:
			setupLog.Error("--webhook-cert-mode must be cert-manager or self-signed", "error", errors.New("invalid flag"), "got", *c.WebhookCertMode)
			return 1
		}
	}

	runner := &managerRunner{
		flags:         c,
		env:           env,
		restCfg:       restCfg,
		logger:        logger,
		rollbackStore: rollbackStore,
		state:         state,
	}
	supervise(ctx, state, logger, runner.run, superviseDelay, superviseHealthyRun)

	// Await the self-signed renewer's clean exit (bounded) after the manager
	// stops, so a SIGTERM mid-rotation doesn't truncate a caBundle write.
	if webhookRenewerDone != nil {
		select {
		case <-webhookRenewerDone:
		case <-time.After(30 * time.Second):
		}
	}
	return 0
}

// managerRunner holds everything one controller manager is built from. Every
// field outlives a single manager, so the supervisor can call run again after
// one stops — each call builds a fresh manager, wires the reconcilers, the
// webhooks, the CRD watcher and the optional gate and MCP servers into it, and
// blocks in Start until its context ends or it fails.
type managerRunner struct {
	flags         *cliflags.Flags
	env           []string
	restCfg       *rest.Config
	logger        *slog.Logger
	rollbackStore controller.RollbackStore
	state         *opstate.State
}

func (m *managerRunner) run(ctx context.Context) error {
	c := m.flags
	logger := m.logger
	setupLog := logger.With("logger", "setup")

	mgrOpts := buildManagerOptions(c, m.env)
	if mgrOpts.Cache.DefaultNamespaces != nil {
		setupLog.Info("watch scope restricted", "namespaces", parseWatchNamespaces(*c.WatchNamespaces, m.env))
	}
	mgr, err := ctrl.NewManager(m.restCfg, mgrOpts)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	stageSetReconciler := &controller.StageSetReconciler{
		Client:                          mgr.GetClient(),
		ShardCap:                        *c.ShardCap,
		AllowedActionHosts:              []string(*c.AllowedActionHosts),
		NoCrossNamespaceRefs:            *c.NoCrossNamespaceRefs,
		RequireVerifiedMigrationSources: *c.RequireVerifiedMigrationSources,
		RequirePinnedMigrationSources:   *c.RequirePinnedMigrationSources,
		RequireImageVerification:        *c.RequireImageVerification,
		ImageVerifier:                   sigstore.New(sigstore.WithLogger(logger.With("logger", "imageverify")), sigstore.WithTrustedRootPath(*c.ImageVerificationTrustedRoot), sigstore.WithInsecureRegistries([]string(*c.ImageVerificationInsecure))),
		ObjectLevelKMS:                  *c.ObjectLevelKMS,
		DefaultInterval:                 *c.DefaultInterval,
		MaxTeardownWait:                 *c.MaxTeardownWait,
		RollbackStore:                   m.rollbackStore,
	}
	if err := stageSetReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup StageSet controller: %w", err)
	}
	// Engage a producer watch the instant a referenced-but-missing producer CRD
	// is installed, instead of at the next reconcile of a referencing StageSet.
	if err := mgr.Add(&controller.CRDWatcher{
		RestCfg: m.restCfg,
		Engager: stageSetReconciler,
		Logger:  logger.With("logger", "crdwatcher"),
	}); err != nil {
		return fmt.Errorf("add CRD watcher: %w", err)
	}

	if err := (&controller.FleetRolloutReconciler{
		Client:             mgr.GetClient(),
		AllowedActionHosts: []string(*c.AllowedActionHosts),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup FleetRollout controller: %w", err)
	}

	if *c.EnableWebhook {
		if err := (&controller.StageSetValidator{}).SetupWebhookWithManager(mgr); err != nil {
			return fmt.Errorf("setup StageSet webhook: %w", err)
		}
		if err := (&controller.ImageVerificationPolicyValidator{}).SetupWebhookWithManager(mgr); err != nil {
			return fmt.Errorf("setup ImageVerificationPolicy webhook: %w", err)
		}
	}

	if *c.GateAddr != "" {
		gateLog := logger.With("logger", "gate")
		if err := mgr.Add(nonLeaderRunnable(func(ctx context.Context) error {
			mux := http.NewServeMux()
			mux.Handle("/gate/", &gate.Handler{Client: mgr.GetClient(), Logger: gateLog})
			srv := &http.Server{Addr: *c.GateAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			// #nosec G118 -- the manager ctx is already done when this goroutine
			// runs, so graceful shutdown needs a fresh, bounded context.
			go func() {
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
			}()
			// The gate is best-effort: a bind failure (e.g. the port is already
			// taken) must NOT bring the manager down with it. Returning a non-nil
			// error here makes controller-runtime shut the whole manager (and the
			// reconciler) down; log and return nil so the gate stays an isolated,
			// degraded subsystem while reconciliation runs on.
			if serveErr := srv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				gateLog.Error("stage-gate server stopped; the gate endpoint is unavailable but reconciliation continues", "error", serveErr, "addr", *c.GateAddr)
			}
			return nil
		})); err != nil {
			return fmt.Errorf("add stage-gate server: %w", err)
		}
	}

	if *c.EnableMCP {
		mcpLog := logger.With("logger", "mcp")
		if err := mgr.Add(nonLeaderRunnable(func(ctx context.Context) error {
			handler := mcp.NewHTTPHandler(mcp.Config{
				KubeClient:     mgr.GetClient(),
				RunbookBaseURL: controller.RunbookBaseURL,
				AllowMutations: *c.MCPAllowMutations,
				Version:        version,
				Logger:         mcpLog,
				RollbackStore:  m.rollbackStore,
			})
			srv := &http.Server{Addr: *c.MCPAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
			// #nosec G118 -- the manager ctx is already done when this goroutine
			// runs, so graceful shutdown needs a fresh, bounded context.
			go func() {
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
			}()
			// Like the gate, the MCP server is best-effort: a bind failure must
			// NOT bring the manager (and the reconciler) down. Log and return nil
			// so it stays an isolated, degraded subsystem while reconciliation
			// runs on.
			if serveErr := srv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				mcpLog.Error("MCP server stopped; the endpoint is unavailable but reconciliation continues", "error", serveErr, "addr", *c.MCPAddr)
			}
			return nil
		})); err != nil {
			return fmt.Errorf("add MCP server: %w", err)
		}
		mcpLog.Info("MCP server enabled", "addr", *c.MCPAddr, "mutations", *c.MCPAllowMutations)
	}

	// Availability — and with it the readiness probe — flips once the caches
	// have synced, on every replica.
	if err := mgr.Add(&readinessGate{state: m.state, logger: logger}); err != nil {
		return fmt.Errorf("add readiness gate: %w", err)
	}

	// Diagnose an un-syncable watch out of band: if a CRD is missing or the
	// ClusterRole can't list/watch it, log one actionable line per resource until
	// the prerequisite appears. The manager retries the informer meanwhile
	// (cacheSyncTimeout), so this never gates startup — it only explains the wait.
	//
	// The check is scoped to this attempt rather than to the process: the
	// supervisor may build many managers, and a checker per attempt that outlived
	// its manager would accumulate goroutines all logging the same thing.
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	if authz, err := authorizationv1client.NewForConfig(m.restCfg); err != nil {
		setupLog.Error("unable to build authorization client for startup checks; skipping", "error", err)
	} else {
		checker := &startupcheck.Checker{Mapper: mgr.GetRESTMapper(), Review: authz.SelfSubjectAccessReviews(), Logger: logger}
		go checker.LogUntilReady(attemptCtx, watchedResources(), 30*time.Second)
	}

	setupLog.Info("starting manager")
	return mgr.Start(ctx)
}

// serveProbes binds the health-probe endpoint: /healthz is an unconditional 200
// so a degraded controller is never restarted by the kubelet, /readyz tracks
// whether the manager is reconciling, and /manager adds the reason behind a
// negative reading. An empty or "0" address disables the endpoint, matching
// controller-runtime's convention for the same flag.
func serveProbes(addr string, state *opstate.State, logger *slog.Logger) (*http.Server, error) {
	if addr == "" || addr == "0" {
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if state.Available() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("controller caches have not synced"))
	})
	mux.HandleFunc("/manager", managerStatusHandler(state))
	return serveOn(addr, mux, logger.With("logger", "probes"))
}

// serveMetrics binds the Prometheus endpoint over controller-runtime's registry,
// which is where both the controller's own metrics and controller-runtime's are
// registered. An empty or "0" address disables it.
func serveMetrics(addr string, logger *slog.Logger) (*http.Server, error) {
	if addr == "" || addr == "0" {
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	return serveOn(addr, mux, logger.With("logger", "metrics"))
}

// serveOn binds addr and serves h until the server is shut down. The listener is
// bound before returning, so a port already in use is reported to the caller
// rather than surfacing asynchronously.
func serveOn(addr string, h http.Handler, logger *slog.Logger) (*http.Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if serveErr := srv.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("endpoint stopped serving", "error", serveErr, "addr", addr)
		}
	}()
	logger.Info("endpoint listening", "addr", addr)
	return srv, nil
}

// shutdownServer stops srv within a bounded window. A nil server is a disabled
// endpoint and needs no shutdown.
func shutdownServer(srv *http.Server) {
	if srv == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// managerStatus is the body of a /manager response. Reason, Since and Attempts
// are omitted while the manager is available, where they carry nothing an
// operator can act on.
type managerStatus struct {
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Since    string `json:"since,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

// managerStatusHandler reports whether the manager is reconciling, with the
// reason it is not. /readyz answers the kubelet's yes-or-no question; this is
// where an operator or an alert reads why the answer is no.
func managerStatusHandler(state *opstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			// RFC 7231 §6.5.5 requires a 405 to advertise the supported methods.
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"status":"method_not_allowed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if state.Available() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		snap := state.Snapshot()
		body := managerStatus{
			Status:   "unavailable",
			Reason:   snap.Reason,
			Attempts: snap.Attempts,
		}
		if !snap.Since.IsZero() {
			body.Since = snap.Since.UTC().Format(time.RFC3339)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// gracefulShutdownTimeout bounds how long the manager waits for in-flight
// runnables (reconcilers, the cache, the gate/MCP/webhook servers) to drain
// after its context is cancelled. Set explicitly rather than tracking
// controller-runtime's default; the self-signed renewer await in run() runs
// only after Start returns, so it is a separate, later window and never races
// this one.
const gracefulShutdownTimeout = 30 * time.Second

// cacheSyncTimeout is effectively unbounded: a controller-runtime controller
// treats a source that does not sync within this window as fatal and exits the
// process, so the default (2m) turns a missing CRD or an incomplete operator
// ClusterRole into a crash-loop. Waiting indefinitely instead keeps the pod
// alive and retrying the informer, so the failure degrades to "not ready,
// waiting" (see startupcheck for the actionable log) and self-heals the moment
// the CRD is installed or the RBAC is granted — no restart. In a healthy cluster
// the caches sync in well under a second, so this is never reached.
const cacheSyncTimeout = 100 * 365 * 24 * time.Hour

// readinessGate marks the manager available once its caches have synced: Start
// runs only after cache sync, and as a non-leader-election runnable it runs on
// every replica. That reading drives /readyz and stageset_manager_available, so
// a pod whose informers cannot sync — a missing CRD, an incomplete ClusterRole —
// stays NotReady and alertable rather than falsely Ready, while /healthz stays
// green so it is never restarted. Mirrors jaas's readinessSignal.
type readinessGate struct {
	state  *opstate.State
	logger *slog.Logger
}

func (*readinessGate) NeedLeaderElection() bool { return false }

func (g *readinessGate) Start(ctx context.Context) error {
	if g.state.MarkAvailable() && g.logger != nil {
		g.logger.Info("manager available", "attempt", g.state.Snapshot().Attempts)
	}
	<-ctx.Done()
	return nil
}

// watchedResources are the CRDs the manager keeps informers on. The startup
// preflight names any that is uninstalled or unreadable so the cause of a
// not-ready pod is one clear log line, not raw reflector spam. Flux source kinds
// are omitted: their watches are already gated on the RESTMapper resolving them
// and are optional, so a not-installed source CRD is expected, not a fault.
func watchedResources() []startupcheck.Target {
	const group = "stages.metio.wtf"
	kinds := map[string]string{
		"StageSet":       "stagesets",
		"StageInventory": "stageinventories",
		"StageLedger":    "stageledgers",
		"FleetRollout":   "fleetrollouts",
	}
	targets := make([]startupcheck.Target, 0, len(kinds))
	for kind, resource := range kinds {
		targets = append(targets, startupcheck.Target{
			GVK:      schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind},
			Group:    group,
			Resource: resource,
		})
	}
	return targets
}

// buildManagerOptions assembles the controller-runtime manager options: leader
// election, the webhook server, the cache watch-scope, and the two guards that
// let a manager be rebuilt — its own metrics and probe servers off, controller
// name validation skipped. It contacts no apiserver, so it is exercised directly
// in tests to pin those guards and that --watch-namespaces lands in
// Cache.DefaultNamespaces as exactly the listed namespaces.
func buildManagerOptions(c *cliflags.Flags, env []string) ctrl.Options {
	mgrOpts := ctrl.Options{
		Scheme: scheme,
		// The binary owns both endpoints (serveMetrics, serveProbes), so the
		// manager binds neither: they have to answer while the manager is the
		// thing that is down, and a rebuilt manager would otherwise fight its
		// predecessor for the ports. "0" is controller-runtime's own "off".
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		LeaderElection:          *c.EnableLeaderElection,
		LeaderElectionID:        "stageset-controller.stages.metio.wtf",
		GracefulShutdownTimeout: new(gracefulShutdownTimeout),
		Controller: ctrlconfig.Controller{
			CacheSyncTimeout: cacheSyncTimeout,
			// controller-runtime keeps controller names for the lifetime of the
			// process and never releases a stopped manager's entry, so a second
			// manager registering "stageset" is rejected. The supervisor builds
			// a new manager whenever the previous one stops — a lost lease, a
			// failure after a healthy period — and that rebuild would otherwise
			// fail permanently, leaving a controller that can never recover
			// without a restart. The check guards against two controllers
			// reporting the same metric, which cannot happen here: exactly one
			// manager is live at a time.
			SkipNameValidation: new(true),
		},
		// Webhook serves on every replica (admission must, even non-leaders);
		// only reconcilers are leader-gated.
		WebhookServer: webhook.NewServer(webhook.Options{Port: *c.WebhookPort, CertDir: *c.WebhookCertDir}),
	}
	if watchNS := parseWatchNamespaces(*c.WatchNamespaces, env); len(watchNS) > 0 {
		// Restrict the manager's informers to the listed namespaces. StageSets
		// and sources outside this set never enter the cache, so the reconciler
		// can't see them even where RBAC would otherwise grant access — the
		// multi-tenant controller-instances pattern (one deployment per
		// tenant-group, disjoint watch sets). The chart pivots RBAC to
		// per-namespace RoleBindings to match.
		nsCache := make(map[string]cache.Config, len(watchNS))
		for _, ns := range watchNS {
			nsCache[ns] = cache.Config{}
		}
		mgrOpts.Cache.DefaultNamespaces = nsCache
	}
	return mgrOpts
}

// buildRollbackStore selects and constructs the optional rollback store from the
// flags, returning (nil, nil) when no backend is configured. Flag-combination
// mistakes — both backends set, or an S3 config with only one of endpoint/bucket
// — return an error so the caller fails fast instead of silently running with
// rollback disabled, in which case a failed deploy would have no store to roll
// back to.
func buildRollbackStore(c *cliflags.Flags) (controller.RollbackStore, error) {
	switch {
	case *c.RBPath != "" && *c.RBS3Endpoint != "":
		return nil, errors.New("--rollback-store-path and --rollback-store-s3-endpoint are mutually exclusive; set only one rollback store")
	case (*c.RBS3Endpoint != "") != (*c.RBS3Bucket != ""):
		return nil, errors.New("--rollback-store-s3-endpoint and --rollback-store-s3-bucket must both be set to enable the S3 rollback store")
	case *c.RBPath != "":
		store, err := rollbackstore.NewFile(*c.RBPath)
		if err != nil {
			return nil, fmt.Errorf("build filesystem rollback store: %w", err)
		}
		return store, nil
	case *c.RBS3Endpoint != "" && *c.RBS3Bucket != "":
		store, err := rollbackstore.NewS3(rollbackstore.S3Config{
			Endpoint: *c.RBS3Endpoint, Bucket: *c.RBS3Bucket, Prefix: *c.RBS3Prefix, Region: *c.RBS3Region,
			UseSSL: *c.RBS3UseSSL, AccessKey: *c.RBS3AccessKey, SecretKey: *c.RBS3SecretKey,
			SessionToken: *c.RBS3SessionToken, Anonymous: *c.RBS3Anonymous,
			SSE: *c.RBS3SSE, SSEKMSKeyID: *c.RBS3SSEKMSKey,
		})
		if err != nil {
			return nil, fmt.Errorf("build S3 rollback store: %w", err)
		}
		return store, nil
	default:
		return nil, nil
	}
}

// provisionSelfSignedWebhookCert generates the in-pod CA + serving cert, writes
// it to certDir, unions this pod's CA into the VWC caBundle (peer-preserving,
// optimistic-concurrency), and starts the rotation renewer. Returns a channel
// closed when the renewer exits.
func provisionSelfSignedWebhookCert(ctx context.Context, logger *slog.Logger, restCfg *rest.Config, in selfsigned.Input, certDir, vwcName string) (<-chan struct{}, error) {
	if in.Namespace == "" {
		return nil, errors.New("webhook self-signed: service namespace is required (set --webhook-service-namespace)")
	}
	if err := os.MkdirAll(certDir, 0o750); err != nil {
		return nil, err
	}
	bundle, err := selfsigned.Generate(in)
	if err != nil {
		return nil, err
	}
	if err := bundle.WriteTo(certDir); err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	vwcs := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	stamp := func(ctx context.Context) error {
		return selfsigned.UpdateVWCCABundle(ctx, vwcs, vwcName, func(cur []byte) []byte {
			return selfsigned.CombineCABundles(cur, bundle.CABundle)
		})
	}
	// The patch is the only step here that needs the apiserver, and an apiserver
	// the pod cannot reach must not end the process: the cert files on disk are
	// already complete, and the probe and metrics endpoints have to stay up to
	// report why the controller is degraded. Admission stays closed until the
	// caBundle lands, so the goroutine below keeps trying and it starts working
	// once the cause — unreachable apiserver, a missing get/update on the named
	// ValidatingWebhookConfiguration — is cleared.
	stamped := true
	if err := stamp(ctx); err != nil {
		stamped = false
		metrics.WebhookCertRenewalFailuresTotal.Inc()
		logger.With("logger", "webhook").Warn("cannot stamp the caBundle yet, retrying in the background", "error", err, "vwc", vwcName)
	}
	renewer := &selfsigned.Renewer{
		Input:     in,
		CertDir:   certDir,
		VWCName:   vwcName,
		VWCClient: vwcs,
		CurrentCA: bundle.CABundle,
		OnFailure: func(error) { metrics.WebhookCertRenewalFailuresTotal.Inc() },
	}
	done := make(chan struct{})
	// #nosec G118 -- the renewer is awaited on the parent ctx and on shutdown
	// via webhookRenewerDone; it is not orphaned.
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				metrics.WebhookCertRenewalFailuresTotal.Inc()
				logger.With("logger", "webhook").Error("self-signed cert renewer panicked", "error", errors.New("renewer panic"), "panic", p)
			}
		}()
		if !stamped && retryCABundle(ctx, logger, vwcName, stamp) != nil {
			// Only ctx expiry ends the retry, which means the process is
			// shutting down; there is nothing left for the renewer to rotate.
			return
		}
		_ = renewer.Run(ctx)
	}()
	return done, nil
}

// retryCABundle re-runs the caBundle patch until it lands or ctx ends, backing
// off between attempts. It returns nil once the patch succeeds and ctx.Err()
// when the process is shutting down.
func retryCABundle(ctx context.Context, logger *slog.Logger, vwcName string, stamp func(context.Context) error) error {
	const (
		baseDelay = 5 * time.Second
		maxDelay  = 2 * time.Minute
	)
	webhookLog := logger.With("logger", "webhook")
	delay := baseDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if err := stamp(ctx); err == nil {
			webhookLog.Info("caBundle stamped", "vwc", vwcName)
			return nil
		} else if ctx.Err() == nil {
			metrics.WebhookCertRenewalFailuresTotal.Inc()
			webhookLog.Warn("cannot stamp the caBundle, admission stays closed", "error", err, "vwc", vwcName, "retryIn", delay)
		}
		delay = min(delay*2, maxDelay)
	}
}

// inClusterNamespace reads the pod's namespace from the mounted ServiceAccount
// token; empty when not running in-cluster.
func inClusterNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return string(data)
}

// parseWatchNamespaces splits a comma-separated list into namespace names,
// falling back to STAGESET_WATCH_NAMESPACES in env when the flag is empty.
// Both empty yields nil — the cluster-wide watch. Entries are trimmed and
// empty entries (trailing/double comma) dropped; DNS-1123 validation is left
// to the apiserver, which rejects malformed names when the cache lists them.
func parseWatchNamespaces(flagValue string, env []string) []string {
	raw := strings.TrimSpace(flagValue)
	if raw == "" {
		for _, e := range env {
			if k, v, ok := strings.Cut(e, "="); ok && k == "STAGESET_WATCH_NAMESPACES" {
				raw = strings.TrimSpace(v)
				break
			}
		}
	}
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// nonLeaderRunnable adapts a function into a manager.Runnable that runs on every
// replica regardless of leader election. A bare manager.RunnableFunc is not a
// LeaderElectionRunnable, so controller-runtime defaults it into the leader-only
// group — which would bind the gate and MCP HTTP endpoints only on the elected
// leader while the readiness probe routes Service traffic to every pod, so
// requests landing on a non-leader would be refused. These endpoints depend only
// on the cache-backed client (available on every replica), so they must not be
// leader-gated.
type nonLeaderRunnable func(context.Context) error

func (r nonLeaderRunnable) Start(ctx context.Context) error { return r(ctx) }
func (r nonLeaderRunnable) NeedLeaderElection() bool        { return false }

var (
	_ manager.Runnable               = nonLeaderRunnable(nil)
	_ manager.LeaderElectionRunnable = nonLeaderRunnable(nil)
)
