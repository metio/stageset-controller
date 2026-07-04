// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

var (
	sharedEnvOnce sync.Once
	sharedEnv     *envtest.Environment
	sharedCfg     *rest.Config
	sharedErr     error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedEnv != nil {
		_ = sharedEnv.Stop()
	}
	os.Exit(code)
}

// envtestConfig lazily boots a shared envtest environment loaded with the
// StageSet/StageInventory CRDs plus a stub ExternalArtifact CRD. Tests skip
// cleanly when the envtest assets are unavailable.
func envtestConfig(t testing.TB) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets unavailable (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}
	sharedEnvOnce.Do(startSharedEnv)
	if sharedErr != nil {
		t.Fatalf("envtest start failed: %v", sharedErr)
	}
	return sharedCfg
}

func startSharedEnv() {
	crdDir, err := repoCRDDir()
	if err != nil {
		sharedErr = err
		return
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
		CRDs:                  []*apiextv1.CustomResourceDefinition{externalArtifactStubCRD(), producerStubCRD()},
	}
	cfg, err := env.Start()
	if err != nil {
		sharedErr = fmt.Errorf("envtest.Start: %w", err)
		return
	}
	sharedEnv = env
	sharedCfg = cfg
}

func repoCRDDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	origin := dir
	for {
		cand := filepath.Join(dir, "config", "crd")
		if fi, statErr := os.Stat(cand); statErr == nil && fi.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("config/crd not found walking up from %s", origin)
		}
		dir = parent
	}
}

func externalArtifactStubCRD() *apiextv1.CustomResourceDefinition {
	gvk := artifact.ExternalArtifactGVK
	preserve := true
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "externalartifacts." + gvk.Group},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: gvk.Group,
			Names: apiextv1.CustomResourceDefinitionNames{
				Kind:     gvk.Kind,
				ListKind: gvk.Kind + "List",
				Plural:   "externalartifacts",
				Singular: "externalartifact",
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name:    gvk.Version,
				Served:  true,
				Storage: true,
				Subresources: &apiextv1.CustomResourceSubresources{
					Status: &apiextv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextv1.JSONSchemaProps{
							"spec":   {Type: "object", XPreserveUnknownFields: &preserve},
							"status": {Type: "object", XPreserveUnknownFields: &preserve},
						},
					},
				},
			}},
		},
	}
}

// producerStubCRD registers a minimal JsonnetSnippet CRD so tests can create
// producer CRs an ExternalArtifact back-references (for --with-source).
func producerStubCRD() *apiextv1.CustomResourceDefinition {
	preserve := true
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "jsonnetsnippets.jaas.metio.wtf"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "jaas.metio.wtf",
			Names: apiextv1.CustomResourceDefinitionNames{
				Kind:     "JsonnetSnippet",
				ListKind: "JsonnetSnippetList",
				Plural:   "jsonnetsnippets",
				Singular: "jsonnetsnippet",
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name:    "v1",
				Served:  true,
				Storage: true,
				Schema: &apiextv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextv1.JSONSchemaProps{
							"spec":   {Type: "object", XPreserveUnknownFields: &preserve},
							"status": {Type: "object", XPreserveUnknownFields: &preserve},
						},
					},
				},
			}},
		},
	}
}

// testClient builds a controller-runtime client against the shared envtest
// apiserver using the CLI's own scheme.
func testClient(t testing.TB, cfg *rest.Config) client.Client {
	t.Helper()
	c, err := client.New(cfg, client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// runCLI executes the whole command tree against an injected envtest config,
// returning stdout, stderr, and the process exit code.
func runCLI(t testing.TB, cfg *rest.Config, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	o := &options{
		streams:            genericiooptions.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errb},
		configFlags:        genericclioptions.NewConfigFlags(true),
		restConfigOverride: cfg,
	}
	code = run(context.Background(), o, args)
	return out.String(), errb.String(), code
}

// makeNamespace creates a uniquely-named namespace and returns its name.
func makeNamespace(t testing.TB, c client.Client, base string) string {
	t.Helper()
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	name := fmt.Sprintf("%s-%d", base, namespaceCounter.next())
	ns.SetName(name)
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return name
}

// makeStageSet creates a minimal valid StageSet (one stage) in ns.
func makeStageSet(t testing.TB, c client.Client, ns, name string) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "first",
				SourceRef: stagesv1.SourceReference{Name: name + "-artifact"},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return ss
}

// namespaceCounter hands out monotonically increasing suffixes so parallel
// tests never collide on a namespace name.
var namespaceCounter counter

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}
