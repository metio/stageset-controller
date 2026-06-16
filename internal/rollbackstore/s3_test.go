// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package rollbackstore

import "testing"

func TestServerSideEncryption(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		kmsKey    string
		wantNil   bool
		wantType  string // encrypt.Type string, when non-nil
		wantError bool
	}{
		{name: "empty is no SSE", mode: "", wantNil: true},
		{name: "none is no SSE", mode: "none", wantNil: true},
		{name: "s3 selects SSE-S3", mode: "s3", wantType: "S3"},
		{name: "kms with key", mode: "kms", kmsKey: "arn:aws:kms:eu-west-1:111:key/abc", wantType: "KMS"},
		{name: "kms without key uses bucket default", mode: "kms", wantType: "KMS"},
		{name: "unknown mode errors", mode: "aes", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sse, err := serverSideEncryption(tt.mode, tt.kmsKey)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error for mode %q, got nil", tt.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNil {
				if sse != nil {
					t.Fatalf("expected nil ServerSide, got %v", sse)
				}
				return
			}
			if sse == nil {
				t.Fatalf("expected non-nil ServerSide for mode %q", tt.mode)
			}
			if got := string(sse.Type()); got != tt.wantType {
				t.Errorf("Type() = %q, want %q", got, tt.wantType)
			}
		})
	}
}

// NewS3 must reject an unknown SSE mode at construction, before any network use.
func TestNewS3_RejectsBadSSE(t *testing.T) {
	_, err := NewS3(S3Config{Endpoint: "s3.example.com", Bucket: "b", SSE: "bogus"})
	if err == nil {
		t.Fatal("expected NewS3 to reject an unknown SSE mode")
	}
}
