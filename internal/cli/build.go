// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/diffrender"
	"github.com/metio/stageset-controller/internal/preview"
)

type buildOptions struct {
	name        string
	stages      []string
	sourceDirs  []string
	showSecrets bool
	asTenant    bool
}

func newBuildCommand(o *options) *cobra.Command {
	opts := buildOptions{}
	cmd := &cobra.Command{
		Use:   "build NAME",
		Short: "Render a StageSet's manifests to stdout",
		Long: "Render the manifests a StageSet's stages produce, using the controller's own resolve→fetch→build path. " +
			"Use --source-dir to render from a local artifact tree when the in-cluster storage is unreachable. " +
			"Secret values are masked unless --show-secrets is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runtimeErr(runBuild(cmd.Context(), o, opts))
		},
	}
	cmd.Flags().StringSliceVar(&opts.stages, "stage", nil, "Render only the named stage(s); repeatable.")
	cmd.Flags().StringArrayVar(&opts.sourceDirs, "source-dir", nil, "Local artifact tree as [STAGE=]PATH; repeatable. Skips the cluster fetch.")
	cmd.Flags().BoolVar(&opts.showSecrets, "show-secrets", false, "Reveal Secret values instead of masking them.")
	cmd.Flags().BoolVar(&opts.asTenant, "as-tenant", false, "Render each stage as its effective serviceAccountName (the stage's own, else spec.serviceAccountName) — the identity the controller uses.")
	return cmd
}

func runBuild(ctx context.Context, o *options, opts buildOptions) error {
	sourceDirs, err := parseSourceDirs(opts.sourceDirs)
	if err != nil {
		return err
	}

	c, _, err := o.newClient()
	if err != nil {
		return err
	}
	var ss stagesv1.StageSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace(), Name: opts.name}, &ss); err != nil {
		return err
	}

	stages, err := preview.SelectStages(&ss, opts.stages)
	if err != nil {
		return err
	}

	// spec.decryption: decrypt where the controller does, or the render below
	// would emit ciphertext the controller never applies.
	dec, err := o.stageSetDecryptor(ctx, c, opts.asTenant, &ss)
	if err != nil {
		return err
	}

	// Each stage renders under its effective ServiceAccount (its own
	// serviceAccountName, else the StageSet default), so a preview reflects what
	// that stage's identity can read. Without --as-tenant every stage renders with
	// the CLI's own credentials. Engines are cached per effective SA.
	engines := map[string]*preview.Engine{}
	engineFor := func(sa string) (*preview.Engine, error) {
		key := ""
		renderClient := c
		if opts.asTenant && sa != "" {
			key = sa
			if e, ok := engines[key]; ok {
				return e, nil
			}
			ic, ierr := o.impersonatedClient(ss.Namespace, sa)
			if ierr != nil {
				return nil, ierr
			}
			renderClient = ic
		} else if e, ok := engines[key]; ok {
			return e, nil
		}
		engine := preview.NewEngine(renderClient, false)
		engine.SourceDirs = sourceDirs
		engine.Decryptor = dec
		engines[key] = engine
		return engine, nil
	}

	masker := diffrender.NewSecretMasker(opts.showSecrets)
	first := true
	for i := range stages {
		sa := stages[i].ServiceAccountName
		if sa == "" {
			sa = ss.Spec.ServiceAccountName
		}
		engine, eerr := engineFor(sa)
		if eerr != nil {
			return eerr
		}
		render, err := engine.RenderStage(ctx, &ss, &stages[i])
		if err != nil {
			return err
		}
		out, err := diffrender.RenderManifests(render.Objects, masker)
		if err != nil {
			return err
		}
		if out == "" {
			continue
		}
		if !first {
			fmt.Fprintln(o.streams.Out, "---")
		}
		first = false
		fmt.Fprint(o.streams.Out, out)
	}
	return nil
}

// parseSourceDirs turns repeated [STAGE=]PATH entries into a stage→dir map. A
// bare PATH uses the empty-string key, applying to every stage without its own
// entry.
func parseSourceDirs(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, e := range entries {
		stage, path, found := strings.Cut(e, "=")
		if !found {
			stage, path = "", e
		}
		if path == "" {
			return nil, fmt.Errorf("invalid --source-dir %q: empty path", e)
		}
		if _, dup := out[stage]; dup {
			return nil, fmt.Errorf("invalid --source-dir: stage %q given twice", stage)
		}
		out[stage] = path
	}
	return out, nil
}
