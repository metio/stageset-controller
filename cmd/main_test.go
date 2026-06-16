// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"reflect"
	"testing"
)

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
