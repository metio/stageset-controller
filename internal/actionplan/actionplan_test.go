// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actionplan

import (
	"errors"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestActionScopes_CoversPreAndPost(t *testing.T) {
	stage := &stagesv1.Stage{Actions: &stagesv1.StageActions{
		Pre:       []stagesv1.Action{{Name: "check"}, {Name: "upgrade", Scope: stagesv1.ScopeVersion}},
		Post:      []stagesv1.Action{{Name: "notify", Scope: stagesv1.ScopeRevision}},
		OnFailure: []stagesv1.Action{{Name: "cleanup"}}, // excluded — no scope there
	}}
	got := ActionScopes(stage)
	want := map[string]stagesv1.ActionScope{
		"check":   stagesv1.ScopeRevision, // "" defaults to Revision
		"upgrade": stagesv1.ScopeVersion,
		"notify":  stagesv1.ScopeRevision,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scopes = %v, want %v", got, want)
	}
	if _, has := got["cleanup"]; has {
		t.Error("onFailure action leaked into the scope map")
	}
}

// ClassifyAnchor is the pure heart of the anchor feature; the table covers every
// branch, including the unreadable (fail-open) one that is awkward to provoke
// through envtest.
func TestClassifyAnchor(t *testing.T) {
	anchored := &stagesv1.LedgerCompletion{Anchor: &stagesv1.AnchorWitness{UID: "u1"}}
	unanchored := &stagesv1.LedgerCompletion{}
	withUID := func(uid string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetUID(types.UID(uid))
		return o
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, "db")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "db", errors.New("nope"))

	tests := []struct {
		name string
		c    *stagesv1.LedgerCompletion
		obj  *unstructured.Unstructured
		err  error
		want AnchorState
	}{
		{"unanchored is always ok", unanchored, nil, nil, AnchorOK},
		{"present with matching uid is ok", anchored, withUID("u1"), nil, AnchorOK},
		{"present with changed uid is gone", anchored, withUID("u2"), nil, AnchorGone},
		{"confirmed absent is gone", anchored, nil, notFound, AnchorGone},
		{"forbidden is unreadable (fail open)", anchored, nil, forbidden, AnchorUnreadable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAnchor(tc.c, tc.obj, tc.err); got != tc.want {
				t.Fatalf("ClassifyAnchor = %v, want %v", got, tc.want)
			}
		})
	}
}
