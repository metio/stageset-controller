// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// newToken mints an opaque, ever-changing reconcile-request token. Its only
// requirement is to differ from the previously handled value; a timestamp
// satisfies that and is human-readable.
func newToken() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// fieldManager attributes the annotation writes this CLI makes, distinct from
// the controller's apply manager so the request write never fights field
// ownership.
const reconcileFieldManager = "stagesetctl"

// reconcileStageAnnotation and updateNowAnnotation mirror the controller's
// annotation contract; the CLI writes them, the controller acts on them.
const (
	requestedAtAnnotation    = fluxmeta.ReconcileRequestAnnotation
	updateNowAnnotation      = "stages.metio.wtf/update-now"
	reconcileStageAnnotation = "stages.metio.wtf/reconcile-stage"
)

type reconcileOptions struct {
	name       string
	stage      string
	withSource bool
	updateNow  bool
	force      bool
	wait       bool
	timeout    time.Duration
}

func newReconcileCommand(o *options) *cobra.Command {
	opts := reconcileOptions{timeout: 5 * time.Minute}
	cmd := &cobra.Command{
		Use:   "reconcile NAME",
		Short: "Force an out-of-band reconcile of a StageSet",
		Long: "Request an immediate reconcile by stamping the reconcile.fluxcd.io/requestedAt annotation, the same " +
			"mechanism `flux reconcile` uses. --stage forces a single stage to re-run its actions; --update-now applies " +
			"a window-held rollout immediately; --with-source first re-requests the stage sources.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runtimeErr(runReconcile(cmd.Context(), o, opts))
		},
	}
	cmd.Flags().StringVar(&opts.stage, "stage", "", "Force only this stage to re-run its actions (single-stage reconcile).")
	cmd.Flags().BoolVar(&opts.withSource, "with-source", false, "Also re-request the stage sources (ExternalArtifacts) before reconciling.")
	cmd.Flags().BoolVar(&opts.updateNow, "update-now", false, "Apply a window-held rollout immediately, bypassing update windows.")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Proceed even when the StageSet is suspended.")
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "Wait until the controller reports the request handled.")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 5*time.Minute, "How long to wait with --wait.")
	return cmd
}

func runReconcile(ctx context.Context, o *options, opts reconcileOptions) error {
	c, _, err := o.newClient()
	if err != nil {
		return err
	}
	var ss stagesv1.StageSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace(), Name: opts.name}, &ss); err != nil {
		return err
	}

	if ss.Spec.Suspend && !opts.force {
		return fmt.Errorf("StageSet %q is suspended; it will not act on a reconcile request (use --force to request anyway)", opts.name)
	}
	if opts.stage != "" && !hasSpecStage(&ss, opts.stage) {
		return fmt.Errorf("stage %q not found in StageSet %q", opts.stage, opts.name)
	}

	token := newToken()

	if opts.withSource {
		if err := reconcileSources(ctx, c, &ss, token, o.streams.ErrOut); err != nil {
			return err
		}
	}

	ann := ss.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[requestedAtAnnotation] = token
	if opts.updateNow {
		ann[updateNowAnnotation] = token
	}
	if opts.stage != "" {
		ann[reconcileStageAnnotation] = opts.stage + "@" + token
	}
	ss.SetAnnotations(ann)
	if err := c.Update(ctx, &ss, client.FieldOwner(reconcileFieldManager)); err != nil {
		return err
	}

	what := "StageSet " + opts.name
	if opts.stage != "" {
		what = fmt.Sprintf("stage %q of StageSet %s", opts.stage, opts.name)
	}
	fmt.Fprintf(o.streams.Out, "Reconcile requested for %s (token %s)\n", what, token)

	if opts.wait {
		return waitForReconcile(ctx, c, &ss, opts, token)
	}
	return nil
}

// reconcileSources stamps requestedAt on each stage's ExternalArtifact so an
// upstream re-publish happens before the StageSet re-reconciles. Sources that
// cannot be resolved or patched are reported but do not abort the run.
func reconcileSources(ctx context.Context, c client.Client, ss *stagesv1.StageSet, token string, errOut io.Writer) error {
	seen := map[string]bool{}
	for i := range ss.Spec.Stages {
		ref := ss.Spec.Stages[i].SourceRef
		ns := ref.Namespace
		if ns == "" {
			ns = ss.Namespace
		}
		key := ns + "/" + ref.Name
		if seen[key] {
			continue
		}
		seen[key] = true

		src := &unstructured.Unstructured{}
		src.SetGroupVersionKind(sourceGVK(ref))
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, src); err != nil {
			fmt.Fprintf(errOut, "warning: cannot reconcile source %s: %v\n", key, err)
			continue
		}
		ann := src.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[requestedAtAnnotation] = token
		src.SetAnnotations(ann)
		if err := c.Update(ctx, src, client.FieldOwner(reconcileFieldManager)); err != nil {
			fmt.Fprintf(errOut, "warning: cannot reconcile source %s: %v\n", key, err)
		}
	}
	return nil
}

// waitForReconcile polls until the controller records the token as handled — at
// the StageSet level, or, for a single-stage request, at the stage level.
func waitForReconcile(ctx context.Context, c client.Client, ss *stagesv1.StageSet, opts reconcileOptions, token string) error {
	wctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	key := client.ObjectKeyFromObject(ss)
	err := wait.PollUntilContextCancel(wctx, time.Second, true, func(ctx context.Context) (bool, error) {
		var cur stagesv1.StageSet
		if err := c.Get(ctx, key, &cur); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return reconcileHandled(&cur, opts, token), nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for reconcile of %q: %w", opts.name, err)
	}
	return nil
}

func reconcileHandled(ss *stagesv1.StageSet, opts reconcileOptions, token string) bool {
	if opts.stage != "" {
		for _, st := range ss.Status.Stages {
			if st.Name == opts.stage {
				return st.LastHandledReconcileAt == token
			}
		}
		return false
	}
	if ss.Status.GetLastHandledReconcileRequest() != token {
		return false
	}
	if opts.updateNow && ss.Status.PendingUpdate != nil {
		return false
	}
	return true
}

func hasSpecStage(ss *stagesv1.StageSet, name string) bool {
	for i := range ss.Spec.Stages {
		if ss.Spec.Stages[i].Name == name {
			return true
		}
	}
	return false
}

// sourceGVK returns the GVK to read a stage's source. A direct ExternalArtifact
// ref (the default) is read as such; a producer back-pointer is read at its own
// kind so the request reaches the producer.
func sourceGVK(ref stagesv1.SourceReference) schema.GroupVersionKind {
	if ref.Kind == "" || ref.Kind == "ExternalArtifact" {
		return artifact.ExternalArtifactGVK
	}
	gv, _ := schema.ParseGroupVersion(ref.APIVersion)
	return gv.WithKind(ref.Kind)
}
