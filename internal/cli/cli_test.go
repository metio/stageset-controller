// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// runArgs executes the command tree with no cluster override, for paths that
// fail before any client is built (flag parsing, command lookup, help).
func runArgs(args ...string) (stdout, stderr string, code int) {
	var out, errb bytes.Buffer
	o := &options{
		streams:     genericiooptions.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errb},
		configFlags: genericclioptions.NewConfigFlags(true),
	}
	code = run(context.Background(), o, args)
	return out.String(), errb.String(), code
}

func TestRun_Help_ExitsZero(t *testing.T) {
	stdout, _, code := runArgs("--help")
	if code != exitOK {
		t.Fatalf("--help exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "stagesetctl") {
		t.Errorf("help output missing command name:\n%s", stdout)
	}
}

func TestRun_Version(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })
	SetBuildInfo("2026.6.16", "deadbeef")

	stdout, _, code := runArgs("--version")
	if code != exitOK {
		t.Fatalf("--version exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "2026.6.16") || !strings.Contains(stdout, "deadbeef") {
		t.Errorf("version output missing stamped values:\n%s", stdout)
	}
}

func TestSetBuildInfo_IgnoresEmpty(t *testing.T) {
	origV, origC := version, commit
	t.Cleanup(func() { version, commit = origV, origC })
	SetBuildInfo("1.2.3", "abc")
	SetBuildInfo("", "") // empty must not clobber a previously stamped value
	if version != "1.2.3" || commit != "abc" {
		t.Errorf("empty SetBuildInfo clobbered values: %s/%s", version, commit)
	}
}

func TestRun_UnknownCommand_ExitsUsage(t *testing.T) {
	_, stderr, code := runArgs("frobnicate")
	if code != exitUsage {
		t.Fatalf("unknown command exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("stderr missing 'unknown command':\n%s", stderr)
	}
}

func TestRun_UnknownFlag_ExitsUsage(t *testing.T) {
	_, stderr, code := runArgs("get", "--no-such-flag")
	if code != exitUsage {
		t.Fatalf("unknown flag exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Errorf("stderr missing 'unknown flag':\n%s", stderr)
	}
}

func TestRun_InvalidOutput_ExitsUsage(t *testing.T) {
	// A bad --output value is flag/usage misuse (exit 2), like a flag-parse
	// error: the caller invoked the tool wrong, the tool didn't fail at runtime.
	_, stderr, code := runArgs("get", "-o", "toml")
	if code != exitUsage {
		t.Fatalf("invalid output exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "invalid --output") {
		t.Errorf("stderr missing validation message:\n%s", stderr)
	}
}

func TestRootUse_KubectlPluginName(t *testing.T) {
	orig := osArgs0
	t.Cleanup(func() { osArgs0 = orig })

	osArgs0 = func() string { return "/usr/local/bin/kubectl-stageset" }
	if got := rootUse(); got != "kubectl stageset" {
		t.Errorf("rootUse under kubectl plugin = %q, want %q", got, "kubectl stageset")
	}

	osArgs0 = func() string { return "/usr/local/bin/stagesetctl" }
	if got := rootUse(); got != "stagesetctl" {
		t.Errorf("rootUse standalone = %q, want %q", got, "stagesetctl")
	}
}
