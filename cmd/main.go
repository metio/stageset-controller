// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// The stageset-controller manager: registers the stages.metio.wtf/v1 scheme
// and starts the StageSet reconciler.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	// Embed the IANA time zone database: update-window timeZones resolve via
	// time.LoadLocation, and the distroless static runtime image ships no
	// /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/cliflags"
	"github.com/metio/stageset-controller/internal/controller"
	"github.com/metio/stageset-controller/internal/gate"
	"github.com/metio/stageset-controller/internal/mcp"
	"github.com/metio/stageset-controller/internal/metrics"
	"github.com/metio/stageset-controller/internal/observability"
	"github.com/metio/stageset-controller/internal/rollbackstore"
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
	mgrOpts := buildManagerOptions(c, env)
	if mgrOpts.Cache.DefaultNamespaces != nil {
		setupLog.Info("watch scope restricted", "namespaces", parseWatchNamespaces(*c.WatchNamespaces, env))
	}
	mgr, err := ctrl.NewManager(restCfg, mgrOpts)
	if err != nil {
		setupLog.Error("unable to start manager", "error", err)
		return 1
	}

	if err = (&controller.StageSetReconciler{
		Client:                          mgr.GetClient(),
		ShardCap:                        *c.ShardCap,
		AllowedActionHosts:              []string(*c.AllowedActionHosts),
		NoCrossNamespaceRefs:            *c.NoCrossNamespaceRefs,
		RequireVerifiedMigrationSources: *c.RequireVerifiedMigrationSources,
		RequirePinnedMigrationSources:   *c.RequirePinnedMigrationSources,
		ObjectLevelKMS:                  *c.ObjectLevelKMS,
		DefaultInterval:                 *c.DefaultInterval,
		MaxTeardownWait:                 *c.MaxTeardownWait,
		RollbackStore:                   rollbackStore,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error("unable to create controller", "error", err, "controller", "StageSet")
		return 1
	}

	if err = (&controller.FleetRolloutReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error("unable to create controller", "error", err, "controller", "FleetRollout")
		return 1
	}

	var webhookRenewerDone <-chan struct{}
	if *c.EnableWebhook {
		if err := (&controller.StageSetValidator{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error("unable to create webhook", "error", err, "webhook", "StageSet")
			return 1
		}
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
			setupLog.Error("unable to add stage-gate server", "error", err)
			return 1
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
				RollbackStore:  rollbackStore,
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
			setupLog.Error("unable to add MCP server", "error", err)
			return 1
		}
		mcpLog.Info("MCP server enabled", "addr", *c.MCPAddr, "mutations", *c.MCPAllowMutations)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error("unable to set up health check", "error", err)
		return 1
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error("unable to set up ready check", "error", err)
		return 1
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error("problem running manager", "error", err)
		return 1
	}
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

// buildManagerOptions assembles the controller-runtime manager options from the
// parsed flags: the metrics bind address, health-probe address, leader election,
// the webhook server, and the cache watch-scope. It contacts no apiserver, so it
// is exercised directly in tests to pin that the configured --metrics-bind-address
// reaches metricsserver.Options and that --watch-namespaces lands in
// Cache.DefaultNamespaces as exactly the listed namespaces.
// gracefulShutdownTimeout bounds how long the manager waits for in-flight
// runnables (reconcilers, the cache, the gate/MCP/webhook servers) to drain
// after its context is cancelled. Set explicitly rather than tracking
// controller-runtime's default; the self-signed renewer await in run() runs
// only after Start returns, so it is a separate, later window and never races
// this one.
const gracefulShutdownTimeout = 30 * time.Second

func buildManagerOptions(c *cliflags.Flags, env []string) ctrl.Options {
	mgrOpts := ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: *c.MetricsAddr},
		HealthProbeBindAddress:  *c.ProbeAddr,
		LeaderElection:          *c.EnableLeaderElection,
		LeaderElectionID:        "stageset-controller.stages.metio.wtf",
		GracefulShutdownTimeout: new(gracefulShutdownTimeout),
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
	if err := selfsigned.UpdateVWCCABundle(ctx, vwcs, vwcName, func(cur []byte) []byte {
		return selfsigned.CombineCABundles(cur, bundle.CABundle)
	}); err != nil {
		return nil, err
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
		_ = renewer.Run(ctx)
	}()
	return done, nil
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
