// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/metio/stageset-controller/internal/cliflags"
)

// flagsFromArgs registers the CLI flags on a throwaway FlagSet and parses args
// into a *cliflags.Flags, mirroring how run() builds its flags.
func flagsFromArgs(t *testing.T, args ...string) *cliflags.Flags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := cliflags.Register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return c
}

func TestBuildRollbackStore(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantNil   bool
		wantErr   bool
		wantInErr string
	}{
		{name: "no backend configured yields no store", args: nil, wantNil: true},
		{
			name:      "both backends set is rejected",
			args:      []string{"--rollback-store-path=/tmp/x", "--rollback-store-s3-endpoint=minio:9000"},
			wantErr:   true,
			wantInErr: "mutually exclusive",
		},
		{
			name:      "s3 endpoint without bucket is rejected",
			args:      []string{"--rollback-store-s3-endpoint=minio:9000"},
			wantErr:   true,
			wantInErr: "must both be set",
		},
		{
			name:      "s3 bucket without endpoint is rejected",
			args:      []string{"--rollback-store-s3-bucket=rb"},
			wantErr:   true,
			wantInErr: "must both be set",
		},
		{
			name: "s3 endpoint and bucket builds a store",
			args: []string{"--rollback-store-s3-endpoint=minio:9000", "--rollback-store-s3-bucket=rb"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, err := buildRollbackStore(flagsFromArgs(t, tc.args...))
			if tc.wantErr != (err != nil) {
				t.Fatalf("buildRollbackStore() err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !strings.Contains(err.Error(), tc.wantInErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantInErr)
				}
				return
			}
			if tc.wantNil != (store == nil) {
				t.Fatalf("buildRollbackStore() store-is-nil = %v, want %v", store == nil, tc.wantNil)
			}
		})
	}
}

// The filesystem backend needs a real, writable directory; use a temp dir so
// the construction path (NewFile → MkdirAll → OpenRoot) is exercised for real.
func TestBuildRollbackStore_FileBackend(t *testing.T) {
	store, err := buildRollbackStore(flagsFromArgs(t, "--rollback-store-path="+t.TempDir()))
	if err != nil {
		t.Fatalf("buildRollbackStore() err = %v", err)
	}
	if store == nil {
		t.Fatal("expected a file-backed store, got nil")
	}
}

// run() must reject invalid rollback configurations with exit 1 before any
// cluster contact, and a flag parse error with exit 2. These cases all return
// before ctrl.GetConfig, so no apiserver is required.
func TestRun_RejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "both rollback backends", args: []string{"--rollback-store-path=/tmp/x", "--rollback-store-s3-endpoint=minio:9000"}, want: 1},
		{name: "s3 endpoint without bucket", args: []string{"--rollback-store-s3-endpoint=minio:9000"}, want: 1},
		{name: "s3 bucket without endpoint", args: []string{"--rollback-store-s3-bucket=rb"}, want: 1},
		{name: "unknown flag", args: []string{"--definitely-not-a-flag"}, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(context.Background(), tc.args, nil, io.Discard); got != tc.want {
				t.Errorf("run() = %d, want %d", got, tc.want)
			}
		})
	}
}
