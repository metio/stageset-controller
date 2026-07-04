// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/decryptor"
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
		keys = decryptor.KeysFromSecretData(sec.Data)
	}

	var opts []decryptor.Option
	// Object-level KMS (opt-in via --object-level-kms): cloud KMS master keys
	// are decrypted with the StageSet's serviceAccountName federated to a cloud
	// identity, instead of the controller's ambient credentials. Requires a
	// serviceAccountName to bind the federation to; without one there is no
	// tenant identity to assume, so the ambient default is kept. The seam
	// (CredentialSource) is overridable so tests inject a fake.
	if r.ObjectLevelKMS && ss.Spec.ServiceAccountName != "" {
		src := r.credentialSource
		if src == nil {
			src = &tenantCredentialSource{
				client:    r.Client,
				namespace: ss.Namespace,
				saName:    ss.Spec.ServiceAccountName,
			}
		}
		opts = append(opts, decryptor.WithCredentialSource(src))
	}
	return decryptor.New(keys, opts...)
}

// decryptFiles applies dec to fetched files in place; a nil dec is a no-op, so
// callers thread it unconditionally.
func decryptFiles(dec *decryptor.Decryptor, files map[string]string) (map[string]string, error) {
	if dec == nil {
		return files, nil
	}
	return dec.DecryptFiles(files)
}
