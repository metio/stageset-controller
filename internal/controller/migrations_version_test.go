// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestValidateVersion(t *testing.T) {
	tests := []struct {
		name    string
		version *stagesv1.VersionSource
		wantErr bool
	}{
		{name: "nil is fine", version: nil},
		{name: "value only", version: &stagesv1.VersionSource{Value: "1.0.0"}},
		{name: "fromObject only", version: &stagesv1.VersionSource{FromObject: &stagesv1.ObjectVersionRef{Stage: "a", Kind: "Deployment", Name: "web"}}},
		{name: "fromArtifact only", version: &stagesv1.VersionSource{FromArtifact: &stagesv1.ArtifactVersionRef{Stage: "a", Path: "VERSION"}}},
		{name: "none set", version: &stagesv1.VersionSource{}, wantErr: true},
		{
			name:    "two set",
			version: &stagesv1.VersionSource{Value: "1.0.0", FromObject: &stagesv1.ObjectVersionRef{Stage: "a", Kind: "Deployment", Name: "web"}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ss := &stagesv1.StageSet{}
			ss.Spec.Version = tt.version
			err := validateVersion(ss)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateVersion err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
