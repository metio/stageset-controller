// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// The stageset-controller manager: registers the stages.metio.wtf/v1 scheme
// and starts the StageSet reconciler.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"strings"
	"time"
	// Embed the IANA time zone database: update-window timeZones resolve via
	// time.LoadLocation, and the distroless static runtime image ships no
	// /usr/share/zoneinfo.
	_ "time/tzdata"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/controller"
	"github.com/metio/stageset-controller/internal/gate"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/metrics"
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
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		inventoryMode        string
		shardCap             int
		allowedActionHosts   stringSlice
		noCrossNamespaceRefs bool
		enableWebhook        bool
		webhookCertMode      string
		webhookCertDir       string
		webhookPort          int
		webhookCertValidity  time.Duration
		webhookServiceName   string
		webhookServiceNS     string
		webhookVWCName       string
		gateAddr             string
		runbookBaseURL       string
		watchNamespaces      string
		defaultInterval      time.Duration
		rbPath               string
		rbS3Endpoint         string
		rbS3Bucket           string
		rbS3Prefix           string
		rbS3Region           string
		rbS3UseSSL           bool
		rbS3AccessKey        string
		rbS3SecretKey        string
		rbS3SessionToken     string
		rbS3Anonymous        bool
		rbS3SSE              string
		rbS3SSEKMSKey        string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&inventoryMode, "inventory-mode", "hybrid", "Inventory strategy: entries, hybrid, or applyset.")
	flag.IntVar(&shardCap, "inventory-shard-cap", inventory.DefaultShardCap, "Maximum entries per StageInventory shard.")
	flag.Var(&allowedActionHosts, "allowed-action-hosts", "Host glob allowed for http actions; repeatable. Loopback and link-local ranges are always denied unless explicitly listed.")
	flag.BoolVar(&noCrossNamespaceRefs, "no-cross-namespace-refs", false, "Deny cross-namespace sourceRef and dependsOn references.")
	flag.BoolVar(&enableWebhook, "enable-webhook", true, "Enable the validating admission webhook for StageSet.")
	flag.StringVar(&webhookCertMode, "webhook-cert-mode", "cert-manager", "How webhook TLS is provisioned: cert-manager (the chart renders a Certificate; cert mounted from a Secret), or self-signed (the controller generates a CA + serving cert in-pod and patches the ValidatingWebhookConfiguration caBundle).")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "Directory holding the webhook tls.crt and tls.key.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the validating webhook server binds to.")
	flag.DurationVar(&webhookCertValidity, "webhook-cert-validity", 365*24*time.Hour, "Validity of the self-signed serving cert; the renewer rotates at validity/3. Operators wanting short-lived material should use cert-manager.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", "stageset-controller-webhook", "Service the webhook is reachable through; builds cert SANs for self-signed mode.")
	flag.StringVar(&webhookServiceNS, "webhook-service-namespace", "", "Namespace of the webhook Service; empty falls back to the in-cluster ServiceAccount namespace.")
	flag.StringVar(&webhookVWCName, "webhook-validating-config-name", "", "Name of the ValidatingWebhookConfiguration whose caBundle to patch. Required for self-signed mode.")
	flag.StringVar(&gateAddr, "gate-bind-address", ":8082", "Address for the read-only Flagger stage-gate endpoint (GET /gate/{namespace}/{stageset}/{stage}). Empty disables it.")
	flag.StringVar(&runbookBaseURL, "runbook-base-url", "", "Optional URL prefix appended to actionable Ready condition messages as (runbook: <base>/<reason>/). Empty disables.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "", "Comma-separated list of namespaces this controller watches. Empty (the default) means cluster-wide. When set, the manager's cache only observes StageSets and sources in these namespaces — the multi-tenant controller-instances pattern. Falls back to STAGESET_WATCH_NAMESPACES env when the flag is empty.")
	flag.DurationVar(&defaultInterval, "default-interval", 10*time.Minute, "Reconcile cadence for StageSets that omit spec.interval.")
	flag.StringVar(&rbPath, "rollback-store-path", "", "Filesystem directory (e.g. an RWX PVC mount) for the optional rollback store. Use an RWX volume for HA replicas. Mutually exclusive with the S3 store.")
	flag.StringVar(&rbS3Endpoint, "rollback-store-s3-endpoint", "", "S3-compatible endpoint for the optional rollback store (host:port). Empty disables the store; rollback falls back to re-fetching the producer artifact.")
	flag.StringVar(&rbS3Bucket, "rollback-store-s3-bucket", "", "Bucket for the rollback store.")
	flag.StringVar(&rbS3Prefix, "rollback-store-s3-prefix", "", "Key prefix within the rollback-store bucket.")
	flag.StringVar(&rbS3Region, "rollback-store-s3-region", "", "Region for the rollback-store bucket.")
	flag.BoolVar(&rbS3UseSSL, "rollback-store-s3-use-ssl", true, "Use TLS for the rollback-store endpoint.")
	flag.StringVar(&rbS3AccessKey, "rollback-store-s3-access-key", "", "Access key; empty engages minio-go's IAM/IRSA discovery chain.")
	flag.StringVar(&rbS3SecretKey, "rollback-store-s3-secret-key", "", "Secret key for the rollback store.")
	flag.StringVar(&rbS3SessionToken, "rollback-store-s3-session-token", "", "Optional session token for the rollback store.")
	flag.BoolVar(&rbS3Anonymous, "rollback-store-s3-anonymous", false, "Use anonymous (unsigned) requests for the rollback store.")
	flag.StringVar(&rbS3SSE, "rollback-store-s3-sse", "s3", "Server-side encryption at rest for stored objects: none, s3 (SSE-S3), or kms (SSE-KMS). The store holds rendered Secret data, so encryption is on by default; set none only for a bucket whose backend cannot honor an SSE header.")
	flag.StringVar(&rbS3SSEKMSKey, "rollback-store-s3-sse-kms-key", "", "KMS key ARN/ID for --rollback-store-s3-sse=kms; empty uses the bucket's default KMS key.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting stageset-controller", "version", version, "commit", commit)

	restCfg := ctrl.GetConfigOrDie()
	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "stageset-controller.stages.metio.wtf",
		// Webhook serves on every replica (admission must, even non-leaders);
		// only reconcilers are leader-gated.
		WebhookServer: webhook.NewServer(webhook.Options{Port: webhookPort, CertDir: webhookCertDir}),
	}
	if watchNS := parseWatchNamespaces(watchNamespaces, os.Environ()); len(watchNS) > 0 {
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
		setupLog.Info("watch scope restricted", "namespaces", watchNS)
	}
	mgr, err := ctrl.NewManager(restCfg, mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	var rollbackStore controller.RollbackStore
	switch {
	case rbPath != "" && rbS3Endpoint != "":
		setupLog.Error(errors.New("set only one rollback store"), "--rollback-store-path and --rollback-store-s3-endpoint are mutually exclusive")
		os.Exit(1)
	case rbPath != "":
		store, serr := rollbackstore.NewFile(rbPath)
		if serr != nil {
			setupLog.Error(serr, "unable to build filesystem rollback store")
			os.Exit(1)
		}
		rollbackStore = store
		setupLog.Info("rollback store enabled", "backend", "filesystem", "path", rbPath)
		// The file store persists rendered output — including Secret data — to
		// this directory. Unlike the S3 backend it cannot set encryption at rest
		// itself, so the volume must provide it.
		setupLog.Info("ensure the rollback-store volume is encrypted at rest (encrypted StorageClass / LUKS / cloud-disk encryption); the file store writes rendered Secret data in the clear", "path", rbPath)
	case rbS3Endpoint != "" && rbS3Bucket != "":
		store, serr := rollbackstore.NewS3(rollbackstore.S3Config{
			Endpoint: rbS3Endpoint, Bucket: rbS3Bucket, Prefix: rbS3Prefix, Region: rbS3Region,
			UseSSL: rbS3UseSSL, AccessKey: rbS3AccessKey, SecretKey: rbS3SecretKey,
			SessionToken: rbS3SessionToken, Anonymous: rbS3Anonymous,
			SSE: rbS3SSE, SSEKMSKeyID: rbS3SSEKMSKey,
		})
		if serr != nil {
			setupLog.Error(serr, "unable to build S3 rollback store")
			os.Exit(1)
		}
		rollbackStore = store
		setupLog.Info("rollback store enabled", "backend", "s3", "endpoint", rbS3Endpoint, "bucket", rbS3Bucket)
	}

	if err = (&controller.StageSetReconciler{
		Client:               mgr.GetClient(),
		InventoryMode:        inventoryMode,
		ShardCap:             shardCap,
		AllowedActionHosts:   allowedActionHosts,
		NoCrossNamespaceRefs: noCrossNamespaceRefs,
		RunbookBaseURL:       runbookBaseURL,
		DefaultInterval:      defaultInterval,
		RollbackStore:        rollbackStore,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StageSet")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	var webhookRenewerDone <-chan struct{}
	if enableWebhook {
		if err := (&controller.StageSetValidator{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "StageSet")
			os.Exit(1)
		}
		switch webhookCertMode {
		case "cert-manager":
			// External tooling provisions tls.crt/tls.key under the cert dir.
		case "self-signed":
			if webhookVWCName == "" {
				setupLog.Error(errors.New("missing flag"), "--webhook-validating-config-name is required for --webhook-cert-mode=self-signed")
				os.Exit(1)
			}
			ns := webhookServiceNS
			if ns == "" {
				ns = inClusterNamespace()
			}
			done, serr := provisionSelfSignedWebhookCert(ctx, restCfg, selfsigned.Input{
				ServiceName: webhookServiceName,
				Namespace:   ns,
				Validity:    webhookCertValidity,
			}, webhookCertDir, webhookVWCName)
			if serr != nil {
				setupLog.Error(serr, "unable to provision self-signed webhook cert")
				os.Exit(1)
			}
			webhookRenewerDone = done
			setupLog.Info("self-signed webhook cert provisioned", "certDir", webhookCertDir, "vwc", webhookVWCName)
		default:
			setupLog.Error(errors.New("invalid flag"), "--webhook-cert-mode must be cert-manager or self-signed", "got", webhookCertMode)
			os.Exit(1)
		}
	}

	if gateAddr != "" {
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			mux := http.NewServeMux()
			mux.Handle("/gate/", &gate.Handler{Client: mgr.GetClient()})
			srv := &http.Server{Addr: gateAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			// #nosec G118 -- the manager ctx is already done when this goroutine
			// runs, so graceful shutdown needs a fresh, bounded context.
			go func() {
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
			}()
			if serveErr := srv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				return serveErr
			}
			return nil
		})); err != nil {
			setupLog.Error(err, "unable to add stage-gate server")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
	// Await the self-signed renewer's clean exit (bounded) after the manager
	// stops, so a SIGTERM mid-rotation doesn't truncate a caBundle write.
	if webhookRenewerDone != nil {
		select {
		case <-webhookRenewerDone:
		case <-time.After(30 * time.Second):
		}
	}
}

// provisionSelfSignedWebhookCert generates the in-pod CA + serving cert, writes
// it to certDir, unions this pod's CA into the VWC caBundle (peer-preserving,
// optimistic-concurrency), and starts the rotation renewer. Returns a channel
// closed when the renewer exits.
func provisionSelfSignedWebhookCert(ctx context.Context, restCfg *rest.Config, in selfsigned.Input, certDir, vwcName string) (<-chan struct{}, error) {
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
				ctrl.Log.WithName("webhook").Error(errors.New("renewer panic"), "self-signed cert renewer panicked", "panic", p)
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
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type stringSlice []string

func (s *stringSlice) String() string { return "" }

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}
