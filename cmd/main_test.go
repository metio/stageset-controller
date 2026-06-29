// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"context"
	"reflect"
	"testing"
)

// The gate and MCP HTTP servers must run on every replica, not only the leader,
// so their runnable must opt out of leader election and still invoke the wrapped
// function.
func TestNonLeaderRunnable(t *testing.T) {
	if nonLeaderRunnable(nil).NeedLeaderElection() {
		t.Error("nonLeaderRunnable must report NeedLeaderElection()==false so it starts on every replica")
	}
	called := false
	r := nonLeaderRunnable(func(context.Context) error { called = true; return nil })
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !called {
		t.Error("Start must invoke the wrapped function")
	}
}

// TestInClusterNamespace covers the not-in-cluster path: with no mounted
// ServiceAccount namespace file present, the lookup returns the empty string
// rather than erroring, so run() falls back to --webhook-service-namespace.
func TestInClusterNamespace_EmptyWhenNotInCluster(t *testing.T) {
	if got := inClusterNamespace(); got != "" {
		t.Errorf("inClusterNamespace() outside a pod = %q, want empty", got)
	}
}

func TestParseWatchNamespaces(t *testing.T) {
	tests := []struct {
		name string
		flag string
		env  []string
		want []string
	}{
		{name: "both empty is cluster-wide", flag: "", env: nil, want: nil},
		{name: "single namespace", flag: "team-a", want: []string{"team-a"}},
		{name: "comma separated", flag: "team-a,team-b,team-c", want: []string{"team-a", "team-b", "team-c"}},
		{name: "trims whitespace", flag: " team-a , team-b ", want: []string{"team-a", "team-b"}},
		{name: "drops empty entries", flag: "team-a,,team-b,", want: []string{"team-a", "team-b"}},
		{
			name: "falls back to env when flag empty",
			flag: "",
			env:  []string{"PATH=/bin", "STAGESET_WATCH_NAMESPACES=team-a,team-b"},
			want: []string{"team-a", "team-b"},
		},
		{
			name: "flag wins over env",
			flag: "team-x",
			env:  []string{"STAGESET_WATCH_NAMESPACES=team-a"},
			want: []string{"team-x"},
		},
		{
			name: "empty env value stays cluster-wide",
			flag: "",
			env:  []string{"STAGESET_WATCH_NAMESPACES="},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWatchNamespaces(tt.flag, tt.env)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseWatchNamespaces(%q, %v) = %v, want %v", tt.flag, tt.env, got, tt.want)
			}
		})
	}
}
