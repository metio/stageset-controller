// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"flag"
	"io"
	"reflect"
	"sort"
	"testing"

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
