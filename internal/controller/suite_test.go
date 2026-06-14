// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

var externalArtifactGVK = schema.GroupVersionKind{
	Group:   "source.toolkit.fluxcd.io",
	Version: "v1",
	Kind:    "ExternalArtifact",
}

var (
	sharedEnvOnce sync.Once
	sharedEnv     *envtest.Environment
	sharedCfg     *rest.Config
	sharedErr     error
)

// TestMain stops the shared envtest apiserver+etcd after the package's tests.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedEnv != nil {
		_ = sharedEnv.Stop()
	}
	os.Exit(code)
}

// envtestConfig lazily boots a shared envtest environment loaded with the
// generated StageSet/StageInventory CRDs plus a stub ExternalArtifact CRD
// (source-controller is not a build dependency). Tests skip cleanly when
// envtest assets are unavailable.
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
		CRDs:                  []*apiextv1.CustomResourceDefinition{externalArtifactStubCRD()},
	}
	cfg, err := env.Start()
	if err != nil {
		sharedErr = fmt.Errorf("envtest.Start: %w", err)
		return
	}
	sharedEnv = env
	sharedCfg = cfg
}

// repoCRDDir walks up from the test's working directory to the generated CRD
// manifests under config/crd.
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

func testScheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgo AddToScheme: %v", err)
	}
	if err := stagesv1.AddToScheme(s); err != nil {
		t.Fatalf("stagesv1 AddToScheme: %v", err)
	}
	s.AddKnownTypeWithName(externalArtifactGVK, &unstructured.Unstructured{})
	listGVK := externalArtifactGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

func testClient(t testing.TB) client.Client {
	t.Helper()
	c, err := client.New(envtestConfig(t), client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// externalArtifactStubCRD is a minimal source.toolkit.fluxcd.io/v1
// ExternalArtifact CRD (open spec+status, status subresource) — enough for
// envtest to store the objects the resolver reads as Unstructured.
func externalArtifactStubCRD() *apiextv1.CustomResourceDefinition {
	preserve := true
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "externalartifacts." + externalArtifactGVK.Group},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: externalArtifactGVK.Group,
			Names: apiextv1.CustomResourceDefinitionNames{
				Kind:     externalArtifactGVK.Kind,
				ListKind: externalArtifactGVK.Kind + "List",
				Plural:   "externalartifacts",
				Singular: "externalartifact",
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name:    externalArtifactGVK.Version,
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
