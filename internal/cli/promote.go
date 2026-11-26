// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// promoteAnnotation mirrors the controller's contract: the CLI stamps it, the
// controller acts on it. Value is "<stage>@<token>", honored once per new token.
const promoteAnnotation = "stages.metio.wtf/promote"

// promoteRevisionAnnotation binds the token to the revision the stage was held
// at when the CLI read it, so the controller refuses the token should the stage
// have moved on to another revision in between.
const promoteRevisionAnnotation = "stages.metio.wtf/promote-revision"

type promoteOptions struct {
	name    string
	stage   string
	force   bool
	wait    bool
	timeout time.Duration
}

func newPromoteCommand(o *options) *cobra.Command {
	opts := promoteOptions{timeout: 5 * time.Minute}
	cmd := &cobra.Command{
		Use:   "promote NAME --stage STAGE",
		Short: "Promote a stage past its promotion gate",
		Long: "Advance a StageSet past the named stage's promotion gate by stamping the stages.metio.wtf/promote " +
			"annotation. This ends a soak early or satisfies a requireManualPromotion gate, letting the rollout " +
			"continue to the next stage. Honored once per invocation (a fresh token each time). The stage must be " +
			"held at its promotion gate; the token is bound to the revision it is held at, and the controller " +
			"refuses it for any other revision.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runtimeErr(runPromote(cmd.Context(), o, opts))
		},
	}
	cmd.Flags().StringVar(&opts.stage, "stage", "", "Name of the stage to promote (required).")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Proceed even when the StageSet is suspended.")
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "Wait until the controller reports the stage promoted.")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 5*time.Minute, "How long to wait with --wait.")
	return cmd
}

func runPromote(ctx context.Context, o *options, opts promoteOptions) error {
	if opts.stage == "" {
		return fmt.Errorf("--stage is required")
	}
	c, _, err := o.newClient()
	if err != nil {
		return err
	}
	var ss stagesv1.StageSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace(), Name: opts.name}, &ss); err != nil {
		return err
	}
	stage := specStage(&ss, opts.stage)
	if stage == nil {
		return fmt.Errorf("stage %q not found in StageSet %q", opts.stage, opts.name)
	}
	if stage.Promotion == nil {
		return fmt.Errorf("stage %q has no promotion gate (spec.stages[].promotion); nothing to promote", opts.stage)
	}
	if ss.Spec.Suspend && !opts.force {
		return fmt.Errorf("StageSet %q is suspended; it will not act on a promotion (use --force to request anyway)", opts.name)
	}

	revision, err := heldRevision(&ss, opts.stage)
	if err != nil {
		return err
	}

	token := newToken()
	ann := ss.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[promoteAnnotation] = opts.stage + "@" + token
	ann[promoteRevisionAnnotation] = revision
	ss.SetAnnotations(ann)
	// The update carries the resourceVersion read above, so it conflicts rather
	// than lands when the controller rewrote the status in the meantime.
	if err := c.Update(ctx, &ss, client.FieldOwner(reconcileFieldManager)); err != nil {
		return err
	}
	fmt.Fprintf(o.streams.Out, "Promotion requested for stage %q of StageSet %s at revision %s (token %s)\n", opts.stage, opts.name, revision, token)

	if opts.wait {
		return waitForPromote(ctx, c, &ss, opts, token)
	}
	return nil
}

// heldRevision returns the revision the stage is held at its promotion gate
// for, or an error naming where the stage stands when it is not held: the
// controller refuses a token for a stage that is not held.
func heldRevision(ss *stagesv1.StageSet, stage string) (string, error) {
	for i := range ss.Status.Stages {
		st := &ss.Status.Stages[i]
		if st.Name != stage {
			continue
		}
		if rev := st.HeldRevision(); rev != "" {
			return rev, nil
		}
		phase := string(st.Phase)
		if st.PromotionState != nil && st.PromotionState.Phase != "" {
			phase = string(st.PromotionState.Phase)
		}
		return "", fmt.Errorf("stage %q of StageSet %q is not held at its promotion gate (it is %s at revision %q); the controller would ignore the promotion", stage, ss.Name, phase, st.AppliedRevision)
	}
	return "", fmt.Errorf("stage %q of StageSet %q has not reached its promotion gate yet; the controller would ignore the promotion", stage, ss.Name)
}

// waitForPromote polls until the controller records the promote token as handled
// for the stage, and fails as soon as it records the token as ignored.
func waitForPromote(ctx context.Context, c client.Client, ss *stagesv1.StageSet, opts promoteOptions, token string) error {
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
		for _, st := range cur.Status.Stages {
			if st.Name != opts.stage {
				continue
			}
			if ig := st.IgnoredPromotion; ig != nil && ig.Token == token {
				return false, fmt.Errorf("%w: %s", errPromotionIgnored, ig.Message)
			}
			return st.LastHandledPromotion == token, nil
		}
		return false, nil
	})
	if errors.Is(err, errPromotionIgnored) {
		return err
	}
	if err != nil {
		return fmt.Errorf("timed out waiting for promotion of stage %q in %q: %w", opts.stage, opts.name, err)
	}
	return nil
}

// errPromotionIgnored reports that the controller refused the promote token.
var errPromotionIgnored = errors.New("the controller ignored the promotion")
