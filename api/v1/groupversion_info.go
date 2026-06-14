// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package v1 contains the stages.metio.wtf/v1 API group: StageSet and
// StageInventory.
//
// +kubebuilder:object:generate=true
// +groupName=stages.metio.wtf
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "stages.metio.wtf", Version: "v1"}

	// SchemeBuilder uses apimachinery's runtime helper rather than
	// sigs.k8s.io/controller-runtime/pkg/scheme so this package stays free of
	// the controller-runtime dependency — cheap for tools (CRD generation,
	// validators) to import without dragging in the manager runtime.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(
		GroupVersion,
		&StageSet{}, &StageSetList{},
		&StageInventory{}, &StageInventoryList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
