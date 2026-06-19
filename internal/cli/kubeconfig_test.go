// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// writeSplitKubeconfig writes a working kubeconfig for rc across TWO files that
// are each incomplete on their own: the cluster file holds the cluster, context,
// and current-context (but no user), the user file holds only the credentials the
// context references. Only the KUBECONFIG-merge of both yields a usable config —
// the way an operator composes one from several files. It returns the
// list-separator-joined KUBECONFIG value.
func writeSplitKubeconfig(t *testing.T, rc *rest.Config, namespace string) string {
	t.Helper()
	dir := t.TempDir()
	clusterFile := filepath.Join(dir, "cluster.yaml")
	userFile := filepath.Join(dir, "user.yaml")

	cluster := clientcmdapi.NewConfig()
	cluster.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   rc.Host,
		CertificateAuthorityData: rc.CAData,
	}
	cluster.Contexts["envtest"] = &clientcmdapi.Context{
		Cluster:   "envtest",
		AuthInfo:  "envtest-user",
		Namespace: namespace,
	}
	cluster.CurrentContext = "envtest"
	if err := clientcmd.WriteToFile(*cluster, clusterFile); err != nil {
		t.Fatalf("write cluster kubeconfig: %v", err)
	}

	user := clientcmdapi.NewConfig()
	user.AuthInfos["envtest-user"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: rc.CertData,
		ClientKeyData:         rc.KeyData,
	}
	if err := clientcmd.WriteToFile(*user, userFile); err != nil {
		t.Fatalf("write user kubeconfig: %v", err)
	}

	return clusterFile + string(os.PathListSeparator) + userFile
}

// runCLIRealConfig runs the command tree through stagesetctl's real kubeconfig
// loading (no injected rest.Config), so ConfigFlags resolves the cluster from the
// KUBECONFIG env exactly as it would for a user.
func runCLIRealConfig(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	o := &options{
		streams:     genericiooptions.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errb},
		configFlags: genericclioptions.NewConfigFlags(true),
	}
	code = run(context.Background(), o, args)
	return out.String(), errb.String(), code
}

// TestKubeconfig_MultipleFilesMerge proves stagesetctl honors a KUBECONFIG set to
// several files that together form one config: the cluster lives in one file, the
// credentials in another, and only the merge connects. This is the multi-file
// KUBECONFIG workflow.
func TestKubeconfig_MultipleFilesMerge(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "kubeconfigmerge")
	makeStageSet(t, c, ns, "merged-app")

	// Keep the discovery cache out of the real home directory.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeSplitKubeconfig(t, cfg, ns))

	stdout, stderr, code := runCLIRealConfig(t, "get", "merged-app", "--namespace", ns)
	if code != exitOK {
		t.Fatalf("get via merged KUBECONFIG exit = %d, want %d (stderr=%s)\n%s", code, exitOK, stderr, stdout)
	}
	if !strings.Contains(stdout, "merged-app") {
		t.Errorf("expected the StageSet name in output:\n%s", stdout)
	}
}

// TestKubeconfig_IncompleteSingleFileFails is the control: pointing KUBECONFIG at
// only the credentials file — no cluster or current-context — cannot connect, so
// the run fails. It confirms the success above genuinely comes from merging the
// second file in, not from one file being sufficient.
func TestKubeconfig_IncompleteSingleFileFails(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "kubeconfigpartial")
	makeStageSet(t, c, ns, "merged-app")

	merged := writeSplitKubeconfig(t, cfg, ns)
	userOnly := strings.Split(merged, string(os.PathListSeparator))[1]

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", userOnly)

	_, _, code := runCLIRealConfig(t, "get", "merged-app", "--namespace", ns)
	if code == exitOK {
		t.Fatal("get with only the credentials file (no cluster/context) should fail, but succeeded")
	}
}
