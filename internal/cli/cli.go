// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package cli implements the stagesetctl command tree: a client-side companion
// to the StageSet controller. Every command is built around a Run seam that
// takes its IO streams and arguments as parameters and never calls os.Exit, so
// whole commands are exercised from tests with in-memory buffers.
package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// Exit codes follow the diff(1) / kubectl diff convention so GitOps pipelines
// can tell "drift present" from "tool broke".
const (
	exitOK    = 0 // success; for diff, no changes
	exitDiff  = 1 // diff: changes present
	exitUsage = 2 // flag or usage error
	exitError = 3 // runtime failure (connection, RBAC, render)
)

// version and commit are shown by `--version`. They default to development
// values and are stamped for releases via SetBuildInfo, which main wires from
// its own -ldflags-set vars.
var (
	version = "development"
	commit  = "unknown"
)

// SetBuildInfo stamps the version and commit reported by `--version`. main
// calls it once before Run so the release ldflags (-X main.version, -X
// main.commit) reach the command tree.
func SetBuildInfo(v, c string) {
	if v != "" {
		version = v
	}
	if c != "" {
		commit = c
	}
}

// exitErr carries an explicit exit code out of a command's RunE so Run can map
// it without re-deriving meaning from the error text.
type exitErr struct {
	code int
	err  error
}

func (e *exitErr) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e *exitErr) Unwrap() error { return e.err }

// runtimeErr wraps any operational failure as an exit-3 error.
func runtimeErr(err error) error {
	if err == nil {
		return nil
	}
	return &exitErr{code: exitError, err: err}
}

// usageErr wraps a flag/usage misuse as an exit-2 error, matching cobra's own
// flag-parse classification (exitUsage) so a caller can distinguish "tool
// misused" from "tool ran but failed".
func usageErr(err error) error {
	if err == nil {
		return nil
	}
	return &exitErr{code: exitUsage, err: err}
}

// options carries the shared dependencies every subcommand needs. Tests
// construct it with a RESTConfigOverride to point at envtest, bypassing
// kubeconfig discovery entirely.
type options struct {
	streams     genericiooptions.IOStreams
	configFlags *genericclioptions.ConfigFlags

	// restConfigOverride, when set, supersedes kubeconfig resolution. Tests
	// inject envtest's *rest.Config here.
	restConfigOverride *rest.Config
}

// Run builds the root command bound to streams, executes it against args, and
// maps the result to a process exit code. It never calls os.Exit itself.
func Run(ctx context.Context, streams genericiooptions.IOStreams, args []string) int {
	o := &options{
		streams:     streams,
		configFlags: genericclioptions.NewConfigFlags(true),
	}
	return run(ctx, o, args)
}

// run is the testable core: it executes the command tree and translates errors
// to exit codes. Tests call it directly with an options carrying an envtest
// RESTConfigOverride and buffer-backed streams.
func run(ctx context.Context, o *options, args []string) int {
	root := newRootCommand(o)
	root.SetArgs(args)
	root.SetIn(o.streams.In)
	root.SetOut(o.streams.Out)
	root.SetErr(o.streams.ErrOut)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}

	if ee, ok := errors.AsType[*exitErr](err); ok {
		// exitDiff is a clean outcome, not an error: stay quiet.
		if ee.code != exitDiff && ee.err != nil {
			fmt.Fprintf(o.streams.ErrOut, "Error: %v\n", ee.err)
		}
		return ee.code
	}

	// Anything cobra surfaces that we did not classify is a usage error: it
	// originates in flag parsing or command lookup, before any RunE runs.
	fmt.Fprintf(o.streams.ErrOut, "Error: %v\n", err)
	return exitUsage
}

// newRootCommand assembles the cobra tree. SilenceUsage/SilenceErrors keep
// cobra from printing on operational failures; run() owns all error output so
// the format is uniform and testable.
func newRootCommand(o *options) *cobra.Command {
	root := &cobra.Command{
		Use:           rootUse(),
		Short:         "Preview and drive StageSets",
		Long:          "stagesetctl previews what a StageSet would change in the cluster, renders a stage's manifests, forces reconciles, and reports status.",
		Version:       fmt.Sprintf("%s (commit %s)", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	o.configFlags.AddFlags(root.PersistentFlags())
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitErr{code: exitUsage, err: err}
	})

	root.AddCommand(newGetCommand(o))
	root.AddCommand(newBuildCommand(o))
	root.AddCommand(newDiffCommand(o))
	root.AddCommand(newApplyCommand(o))
	root.AddCommand(newReconcileCommand(o))
	root.AddCommand(newPromoteCommand(o))
	root.AddCommand(newLintMigrationsCommand(o))

	return root
}

// rootUse derives the displayed command name from the invoking binary, so help
// reads `kubectl stageset …` when installed as the kubectl-stageset plugin and
// `stagesetctl …` otherwise.
func rootUse() string {
	if invokedAsKubectlPlugin() {
		return "kubectl stageset"
	}
	return "stagesetctl"
}

// --- shared client plumbing ---

// scheme registers the stages.metio.wtf/v1 types plus ExternalArtifact as
// Unstructured, matching the controller and its envtest harness.
func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := stagesv1.AddToScheme(s); err != nil {
		panic(err)
	}
	eaGVK := artifact.ExternalArtifactGVK
	s.AddKnownTypeWithName(eaGVK, &unstructured.Unstructured{})
	listGVK := eaGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

func (o *options) restConfig() (*rest.Config, error) {
	if o.restConfigOverride != nil {
		return o.restConfigOverride, nil
	}
	return o.configFlags.ToRESTConfig()
}

func (o *options) newClient() (client.Client, apimeta.RESTMapper, error) {
	cfg, err := o.restConfig()
	if err != nil {
		return nil, nil, err
	}
	mapper, err := o.restMapper(cfg)
	if err != nil {
		return nil, nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme(), Mapper: mapper})
	if err != nil {
		return nil, nil, err
	}
	return c, mapper, nil
}

// impersonatedClient clones the resolved config to act as the given service
// account (system:serviceaccount:<ns>:<sa>), mirroring how the controller
// renders and applies under spec.serviceAccountName.
func (o *options) impersonatedClient(ns, sa string) (client.Client, error) {
	cfg, err := o.restConfig()
	if err != nil {
		return nil, err
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: fmt.Sprintf("system:serviceaccount:%s:%s", ns, sa),
	}
	mapper, err := o.restMapper(cfg)
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme(), Mapper: mapper})
}

func (o *options) restMapper(cfg *rest.Config) (apimeta.RESTMapper, error) {
	if o.restConfigOverride == nil {
		return o.configFlags.ToRESTMapper()
	}
	// Under an injected config (envtest) the ConfigFlags carry no discovery
	// client, so build a dynamic mapper straight from the rest.Config.
	return apiMapperFor(cfg)
}

// namespace resolves the target namespace: the explicit -n flag wins; under a
// real kubeconfig the context's namespace is the fallback; otherwise "default".
func (o *options) namespace() string {
	if o.configFlags.Namespace != nil && *o.configFlags.Namespace != "" {
		return *o.configFlags.Namespace
	}
	if o.restConfigOverride == nil {
		if ns, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace(); err == nil && ns != "" {
			return ns
		}
	}
	return "default"
}

// invokedAsKubectlPlugin reports whether the binary was launched under the
// kubectl-stageset name (kubectl rewrites argv[0] to the plugin path).
func invokedAsKubectlPlugin() bool {
	return filepath.Base(osArgs0()) == "kubectl-stageset"
}
