// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"flag"
	"io"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/metio/stageset-controller/internal/cliflags"
)

// flagsFor parses args (and supplies env) through the real flag registration so
// the test exercises the same Flags the manager sees in production.
func flagsFor(t *testing.T, args []string) *cliflags.Flags {
	t.Helper()
	fs := flag.NewFlagSet("stageset-controller", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := cliflags.Register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}
	return c
}

// TestBuildManagerOptions_PropagatesMetricsBindAddress pins that the configured
// --metrics-bind-address reaches metricsserver.Options.BindAddress — the wiring
// that decides whether (and where) controller-runtime serves /metrics.
func TestBuildManagerOptions_PropagatesMetricsBindAddress(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"explicit address forwards", []string{"--metrics-bind-address=127.0.0.1:9876"}, "127.0.0.1:9876"},
		{"disabled forwards as \"0\"", []string{"--metrics-bind-address=0"}, "0"},
		{"default is :8080", nil, ":8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := flagsFor(t, tc.args)
			opts := buildManagerOptions(c, nil)
			if opts.Metrics.BindAddress != tc.want {
				t.Errorf("Metrics.BindAddress = %q, want %q", opts.Metrics.BindAddress, tc.want)
			}
		})
	}
}

// The manager's graceful-shutdown timeout must be set explicitly so it doesn't
// silently track controller-runtime's default.
func TestBuildManagerOptions_SetsGracefulShutdownTimeout(t *testing.T) {
	opts := buildManagerOptions(flagsFor(t, nil), nil)
	if opts.GracefulShutdownTimeout == nil {
		t.Fatal("GracefulShutdownTimeout is nil; want it set explicitly")
	}
	if *opts.GracefulShutdownTimeout != gracefulShutdownTimeout {
		t.Errorf("GracefulShutdownTimeout = %s, want %s", *opts.GracefulShutdownTimeout, gracefulShutdownTimeout)
	}
}

// TestBuildManagerOptions_SetsUnboundedCacheSyncTimeout pins the crash-loop
// guard: a controller whose informer cannot sync (missing CRD / RBAC) must wait
// indefinitely and keep the pod alive, not hit the 2m default and exit.
func TestBuildManagerOptions_SetsUnboundedCacheSyncTimeout(t *testing.T) {
	opts := buildManagerOptions(flagsFor(t, nil), nil)
	if opts.Controller.CacheSyncTimeout != cacheSyncTimeout {
		t.Errorf("CacheSyncTimeout = %s, want %s", opts.Controller.CacheSyncTimeout, cacheSyncTimeout)
	}
	if opts.Controller.CacheSyncTimeout < 365*24*time.Hour {
		t.Errorf("CacheSyncTimeout = %s, want effectively unbounded (>= 1 year)", opts.Controller.CacheSyncTimeout)
	}
}

// TestReadinessGate_FlipsAfterStart proves readyz reports not-ready until the
// gate's Start runs (which controller-runtime invokes only after cache sync).
func TestReadinessGate_FlipsAfterStart(t *testing.T) {
	g := &readinessGate{}
	if err := g.check(nil); err == nil {
		t.Fatal("readyz should be not-ready before cache sync")
	}
	go func() { _ = g.Start(t.Context()) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g.check(nil) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("readyz never became ready after Start")
}

// TestBuildManagerOptions_PropagatesWatchNamespaces proves the watch-scope
// behavior at the options layer: the listed namespaces — and only those — land
// in Cache.DefaultNamespaces, the map controller-runtime uses to restrict every
// informer. A StageSet in a namespace absent from this map never enters the
// cache, so the reconciler can't see it; one in a listed namespace does.
func TestBuildManagerOptions_PropagatesWatchNamespaces(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  []string
		want []string // nil == cluster-wide (DefaultNamespaces unset)
	}{
		{"empty is cluster-wide", nil, nil, nil},
		{"single namespace", []string{"--watch-namespaces=team-a"}, nil, []string{"team-a"}},
		{"multiple namespaces", []string{"--watch-namespaces=team-a,team-b,team-c"}, nil, []string{"team-a", "team-b", "team-c"}},
		{"falls back to env", nil, []string{"STAGESET_WATCH_NAMESPACES=team-a,team-b"}, []string{"team-a", "team-b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := flagsFor(t, tc.args)
			opts := buildManagerOptions(c, tc.env)

			if tc.want == nil {
				if opts.Cache.DefaultNamespaces != nil {
					t.Fatalf("cluster-wide expected, but DefaultNamespaces = %v", opts.Cache.DefaultNamespaces)
				}
				return
			}
			got := make([]string, 0, len(opts.Cache.DefaultNamespaces))
			for ns := range opts.Cache.DefaultNamespaces {
				got = append(got, ns)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("DefaultNamespaces keys = %v, want %v", got, want)
			}
			// A namespace outside the watch set must be absent — that absence is
			// exactly what keeps its StageSets out of the cache.
			if _, present := opts.Cache.DefaultNamespaces["unwatched-namespace"]; present {
				t.Errorf("unwatched namespace leaked into DefaultNamespaces")
			}
		})
	}
}
