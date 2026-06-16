// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/decryptor"
)

// SOPS conventions for key material in a Secret: age private keys live under
// data entries suffixed ".agekey", armored PGP private keys under ".asc".
const (
	ageKeySuffix = ".agekey"
	pgpKeySuffix = ".asc"
)

// buildDecryptor constructs a SOPS decryptor from spec.decryption, reading the
// key Secret in the StageSet's namespace under its serviceAccountName — so a
// tenant can only decrypt with key material its ServiceAccount can read.
// Returns (nil, nil) when decryption is not configured.
func (r *StageSetReconciler) buildDecryptor(ctx context.Context, ss *stagesv1.StageSet) (*decryptor.Decryptor, error) {
	if ss.Spec.Decryption == nil {
		return nil, nil
	}
	d := ss.Spec.Decryption
	if d.Provider != "sops" {
		return nil, fmt.Errorf("spec.decryption.provider %q is not supported (only sops)", d.Provider)
	}
	// age and PGP keys come from secretRef (optional — a KMS-only setup decrypts
	// through the controller's ambient cloud credentials instead). The Secret is
	// read in the StageSet's namespace under its serviceAccountName, so a tenant
	// can only use key material its ServiceAccount can read.
	var keys decryptor.Keys
	if d.SecretRef != nil {
		local, _, err := r.targetCluster(ctx, ss.Namespace, ss.Spec.ServiceAccountName, nil)
		if err != nil {
			return nil, fmt.Errorf("decryption: %w", err)
		}
		var sec corev1.Secret
		if err := local.Get(ctx, types.NamespacedName{Namespace: ss.Namespace, Name: d.SecretRef.Name}, &sec); err != nil {
			return nil, fmt.Errorf("decryption: read key secret %q: %w", d.SecretRef.Name, err)
		}
		for k, v := range sec.Data {
			switch {
			case strings.HasSuffix(k, ageKeySuffix):
				keys.Age = append(keys.Age, string(v))
			case strings.HasSuffix(k, pgpKeySuffix):
				keys.PGP = append(keys.PGP, string(v))
			}
		}
	}
	return decryptor.New(keys)
}

// decryptFiles applies dec to fetched files in place; a nil dec is a no-op, so
// callers thread it unconditionally.
func decryptFiles(dec *decryptor.Decryptor, files map[string]string) (map[string]string, error) {
	if dec == nil {
		return files, nil
	}
	return dec.DecryptFiles(files)
}
