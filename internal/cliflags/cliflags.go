// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package cliflags defines the stageset-controller binary's own CLI surface in
// one place so the runtime (cmd/main) and the documentation generator
// (hack/flaggen) derive from a single source. Register declares every flag on a
// flag.FlagSet, co-locates each with its documentation group, and returns the
// typed value pointers main dereferences after parsing; flaggen introspects the
// same FlagSet without dereferencing.
//
// The logging flags (--log-level, --log-format) are registered here like every
// other flag: main reads them to build the slog logger that backs both the
// application's own logs and, via the logr bridge, controller-runtime's.
package cliflags

import (
	"flag"
	"time"

	"github.com/metio/stageset-controller/internal/inventory"
)

// Groups is the ordered list of flag-group section names. The order here is the
// stable emission order for generated documentation (hack/flaggen) and matches
// the section headings in docs/content/installation/configuration.md.
func Groups() []string {
	return []string{
		"Manager and leader election",
		"Watch scope",
		"Reconciliation defaults",
		"Rollback store — filesystem",
		"Rollback store — S3",
		"Metrics and health",
		"Tracing",
		"Webhook and TLS provisioning",
		"Gate endpoint",
		"MCP",
		"Logging",
	}
}

// StringSlice is a repeatable string flag value. Each --flag occurrence appends
// to the slice.
type StringSlice []string

func (s *StringSlice) String() string { return "" }

func (s *StringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// Flags holds pointers to every value registered on the FlagSet. main
// dereferences these after fs.Parse; flaggen never does.
type Flags struct {
	MetricsAddr          *string
	ProbeAddr            *string
	EnableLeaderElection *bool

	WatchNamespaces *string

	ShardCap             *int
	NoCrossNamespaceRefs *bool
	ObjectLevelKMS       *bool
	AllowedActionHosts   *StringSlice
	DefaultInterval      *time.Duration
	MaxTeardownWait      *time.Duration

	RBPath *string

	RBS3Endpoint     *string
	RBS3Bucket       *string
	RBS3Prefix       *string
	RBS3Region       *string
	RBS3UseSSL       *bool
	RBS3AccessKey    *string
	RBS3SecretKey    *string
	RBS3SessionToken *string
	RBS3Anonymous    *bool
	RBS3SSE          *string
	RBS3SSEKMSKey    *string

	TracingEndpoint    *string
	TracingInsecure    *bool
	TracingSampleRatio *float64

	EnableWebhook       *bool
	WebhookCertMode     *string
	WebhookCertDir      *string
	WebhookPort         *int
	WebhookCertValidity *time.Duration
	WebhookServiceName  *string
	WebhookServiceNS    *string
	WebhookVWCName      *string

	GateAddr *string

	MCPAddr           *string
	MCPAllowMutations *bool

	LogLevel  *string
	LogFormat *string
}

// groupByName records each flag's documentation group, populated by Register.
// flaggen reads it via GroupOf to bucket flags into their rendered tables.
var groupByName = map[string]string{}

// GroupOf returns the documentation group a flag was registered under, or the
// empty string when the name is unknown. flaggen uses this to order and bucket
// the generated reference.
func GroupOf(name string) string { return groupByName[name] }

// Register declares every controller flag on fs, co-locates each with its
// documentation group, and returns a struct of the registered value pointers.
func Register(fs *flag.FlagSet) *Flags {
	f := &Flags{
		MetricsAddr:          new(string),
		ProbeAddr:            new(string),
		EnableLeaderElection: new(bool),
		WatchNamespaces:      new(string),
		ShardCap:             new(int),
		NoCrossNamespaceRefs: new(bool),
		ObjectLevelKMS:       new(bool),
		AllowedActionHosts:   &StringSlice{},
		DefaultInterval:      new(time.Duration),
		MaxTeardownWait:      new(time.Duration),
		RBPath:               new(string),
		RBS3Endpoint:         new(string),
		RBS3Bucket:           new(string),
		RBS3Prefix:           new(string),
		RBS3Region:           new(string),
		RBS3UseSSL:           new(bool),
		RBS3AccessKey:        new(string),
		RBS3SecretKey:        new(string),
		RBS3SessionToken:     new(string),
		RBS3Anonymous:        new(bool),
		RBS3SSE:              new(string),
		RBS3SSEKMSKey:        new(string),
		TracingEndpoint:      new(string),
		TracingInsecure:      new(bool),
		TracingSampleRatio:   new(float64),
		EnableWebhook:        new(bool),
		WebhookCertMode:      new(string),
		WebhookCertDir:       new(string),
		WebhookPort:          new(int),
		WebhookCertValidity:  new(time.Duration),
		WebhookServiceName:   new(string),
		WebhookServiceNS:     new(string),
		WebhookVWCName:       new(string),
		GateAddr:             new(string),
		MCPAddr:              new(string),
		MCPAllowMutations:    new(bool),
		LogLevel:             new(string),
		LogFormat:            new(string),
	}

	group := func(name string, g string) string {
		groupByName[name] = g
		return name
	}

	const (
		mgr     = "Manager and leader election"
		watch   = "Watch scope"
		recon   = "Reconciliation defaults"
		rbFile  = "Rollback store — filesystem"
		rbS3    = "Rollback store — S3"
		metrics = "Metrics and health"
		tracing = "Tracing"
		webhook = "Webhook and TLS provisioning"
		gate    = "Gate endpoint"
		mcpg    = "MCP"
		logging = "Logging"
	)

	fs.StringVar(f.MetricsAddr, group("metrics-bind-address", metrics), ":8080", "The address the metric endpoint binds to.")
	fs.StringVar(f.ProbeAddr, group("health-probe-bind-address", mgr), ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(f.EnableLeaderElection, group("leader-elect", mgr), false, "Enable leader election.")
	fs.IntVar(f.ShardCap, group("inventory-shard-cap", recon), inventory.DefaultShardCap, "Maximum entries per StageInventory shard.")
	fs.Var(f.AllowedActionHosts, group("allowed-action-hosts", recon), "Host glob allowed for http actions; repeatable. Loopback and link-local ranges are always denied unless explicitly listed.")
	fs.BoolVar(f.NoCrossNamespaceRefs, group("no-cross-namespace-refs", recon), false, "Deny cross-namespace sourceRef and dependsOn references.")
	fs.BoolVar(f.ObjectLevelKMS, group("object-level-kms", recon), false, "Decrypt SOPS cloud KMS keys with each StageSet's serviceAccountName federated to a cloud identity, instead of the controller's ambient credentials. The tenant ServiceAccount must be federated (IRSA / Workload Identity) to a cloud identity granted KMS decrypt. Off by default (ambient credentials).")
	fs.StringVar(f.TracingEndpoint, group("tracing-endpoint", tracing), "", "OTLP gRPC collector host:port (e.g. otel-collector.observability.svc:4317). Empty disables tracing entirely.")
	fs.BoolVar(f.TracingInsecure, group("tracing-insecure", tracing), false, "Skip TLS when dialing the OTLP collector. Use only for in-cluster collectors that don't terminate TLS themselves.")
	fs.Float64Var(f.TracingSampleRatio, group("tracing-sample-ratio", tracing), 1.0, "TraceID-ratio sampling (0.0..1.0). 1.0 samples every trace.")
	fs.BoolVar(f.EnableWebhook, group("enable-webhook", webhook), true, "Enable the validating admission webhook for StageSet.")
	fs.StringVar(f.WebhookCertMode, group("webhook-cert-mode", webhook), "cert-manager", "How webhook TLS is provisioned: cert-manager (the chart renders a Certificate; cert mounted from a Secret), or self-signed (the controller generates a CA + serving cert in-pod and patches the ValidatingWebhookConfiguration caBundle).")
	fs.StringVar(f.WebhookCertDir, group("webhook-cert-dir", webhook), "/tmp/k8s-webhook-server/serving-certs", "Directory holding the webhook tls.crt and tls.key.")
	fs.IntVar(f.WebhookPort, group("webhook-port", webhook), 9443, "Port the validating webhook server binds to.")
	fs.DurationVar(f.WebhookCertValidity, group("webhook-cert-validity", webhook), 365*24*time.Hour, "Validity of the self-signed serving cert; the renewer rotates at validity/3. Operators wanting short-lived material should use cert-manager.")
	fs.StringVar(f.WebhookServiceName, group("webhook-service-name", webhook), "stageset-controller-webhook", "Service the webhook is reachable through; builds cert SANs for self-signed mode.")
	fs.StringVar(f.WebhookServiceNS, group("webhook-service-namespace", webhook), "", "Namespace of the webhook Service; empty falls back to the in-cluster ServiceAccount namespace.")
	fs.StringVar(f.WebhookVWCName, group("webhook-validating-config-name", webhook), "", "Name of the ValidatingWebhookConfiguration whose caBundle to patch. Required for self-signed mode.")
	fs.StringVar(f.GateAddr, group("gate-bind-address", gate), ":8082", "Address for the read-only Flagger stage-gate endpoint (GET /gate/{namespace}/{stageset}/{stage}). Empty disables it.")
	fs.StringVar(f.MCPAddr, group("mcp-bind-address", mcpg), "", "Address for the MCP (Model Context Protocol) server exposing StageSet introspection tools over streamable HTTP (list_stagesets, get_stageset). Empty disables it.")
	fs.BoolVar(f.MCPAllowMutations, group("mcp-allow-mutations", mcpg), false, "Also expose the gated MCP write tools (reconcile/suspend/resume) in addition to the read tools. Off by default; has no effect unless --mcp-bind-address is set.")
	fs.StringVar(f.WatchNamespaces, group("watch-namespaces", watch), "", "Comma-separated list of namespaces this controller watches. Empty (the default) means cluster-wide. When set, the manager's cache only observes StageSets and sources in these namespaces — the multi-tenant controller-instances pattern. Falls back to STAGESET_WATCH_NAMESPACES env when the flag is empty.")
	fs.DurationVar(f.DefaultInterval, group("default-interval", recon), 10*time.Minute, "Reconcile cadence for StageSets that omit spec.interval.")
	fs.DurationVar(f.MaxTeardownWait, group("max-teardown-wait", recon), time.Hour, "How long a deleting StageSet's finalizer holds while reverse-order teardown keeps failing before it is force-dropped. Past this bound the controller emits a Warning TeardownForced event and removes the finalizer anyway, so a permanently-unreachable target cannot wedge the StageSet in Terminating — at the cost of orphaning objects the failing stage could not delete.")
	fs.StringVar(f.RBPath, group("rollback-store-path", rbFile), "", "Filesystem directory (e.g. an RWX PVC mount) for the optional rollback store. Use an RWX volume for HA replicas. Mutually exclusive with the S3 store.")
	fs.StringVar(f.RBS3Endpoint, group("rollback-store-s3-endpoint", rbS3), "", "S3-compatible endpoint for the optional rollback store (host:port). Empty disables the store; rollback falls back to re-fetching the producer artifact.")
	fs.StringVar(f.RBS3Bucket, group("rollback-store-s3-bucket", rbS3), "", "Bucket for the rollback store.")
	fs.StringVar(f.RBS3Prefix, group("rollback-store-s3-prefix", rbS3), "", "Key prefix within the rollback-store bucket.")
	fs.StringVar(f.RBS3Region, group("rollback-store-s3-region", rbS3), "", "Region for the rollback-store bucket.")
	fs.BoolVar(f.RBS3UseSSL, group("rollback-store-s3-use-ssl", rbS3), true, "Use TLS for the rollback-store endpoint.")
	fs.StringVar(f.RBS3AccessKey, group("rollback-store-s3-access-key", rbS3), "", "Access key; empty engages minio-go's IAM/IRSA discovery chain.")
	fs.StringVar(f.RBS3SecretKey, group("rollback-store-s3-secret-key", rbS3), "", "Secret key for the rollback store.")
	fs.StringVar(f.RBS3SessionToken, group("rollback-store-s3-session-token", rbS3), "", "Optional session token for the rollback store.")
	fs.BoolVar(f.RBS3Anonymous, group("rollback-store-s3-anonymous", rbS3), false, "Use anonymous (unsigned) requests for the rollback store.")
	fs.StringVar(f.RBS3SSE, group("rollback-store-s3-sse", rbS3), "s3", "Server-side encryption at rest for stored objects: none, s3 (SSE-S3), or kms (SSE-KMS). The store holds rendered Secret data, so encryption is on by default; set none only for a bucket whose backend cannot honor an SSE header.")
	fs.StringVar(f.RBS3SSEKMSKey, group("rollback-store-s3-sse-kms-key", rbS3), "", "KMS key ARN/ID for --rollback-store-s3-sse=kms; empty uses the bucket's default KMS key.")
	fs.StringVar(f.LogLevel, group("log-level", logging), "info", "Log level: debug, info, warn, or error.")
	fs.StringVar(f.LogFormat, group("log-format", logging), "json", "Log output format: json or text.")

	return f
}
