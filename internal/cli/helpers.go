// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/decryptor"
	"github.com/metio/stageset-controller/internal/preview"
)

// stageSetDecryptor builds the spec.decryption SOPS decryptor for build, diff,
// and apply, so the client-side render decrypts exactly where the controller
// does (between fetch and build). The key Secret is read with the identity the
// controller uses: the StageSet-level serviceAccountName under --as-tenant,
// else the CLI's own credentials — regardless of any per-stage SA, matching
// the reconcile path. Returns nil when decryption is not configured.
func (o *options) stageSetDecryptor(ctx context.Context, c client.Client, asTenant bool, ss *stagesv1.StageSet) (*decryptor.Decryptor, error) {
	if ss.Spec.Decryption == nil {
		return nil, nil
	}
	readClient := c
	if asTenant && ss.Spec.ServiceAccountName != "" {
		ic, err := o.impersonatedClient(ss.Namespace, ss.Spec.ServiceAccountName)
		if err != nil {
			return nil, err
		}
		readClient = ic
	}
	return preview.BuildDecryptor(ctx, readClient, ss)
}

// osArgs0 is a seam over os.Args[0] so the kubectl-plugin name detection is
// exercisable from tests.
var osArgs0 = func() string {
	if len(os.Args) == 0 {
		return ""
	}
	return os.Args[0]
}

// colorEnabled resolves the --color mode against the output stream: "always"
// forces color on, "never" off, and "auto" (the default) enables it only when
// the stream is a terminal and NO_COLOR is unset.
func colorEnabled(mode string, out io.Writer) (bool, error) {
	switch mode {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "", "auto":
		if os.Getenv("NO_COLOR") != "" {
			return false, nil
		}
		f, ok := out.(*os.File)
		return ok && term.IsTerminal(int(f.Fd())), nil
	default:
		return false, fmt.Errorf("invalid --color %q: want auto, always, or never", mode)
	}
}

// apiMapperFor builds a discovery-backed RESTMapper from a rest.Config, used
// when the ConfigFlags cannot supply one (an injected envtest config).
func apiMapperFor(cfg *rest.Config) (apimeta.RESTMapper, error) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return apiutil.NewDynamicRESTMapper(cfg, httpClient)
}
