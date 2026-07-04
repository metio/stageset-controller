// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package decryptor

import "strings"

// SOPS conventions for key material in a Secret: age private keys live under
// data entries suffixed ".agekey", armored PGP private keys under ".asc".
// Both the controller's reconcile path and the CLI preview read key Secrets
// through this one mapping so the two can never disagree on what counts as a
// key.
const (
	AgeKeySuffix = ".agekey"
	PGPKeySuffix = ".asc"
)

// KeysFromSecretData extracts age and PGP private keys from a Secret's data
// by the suffix conventions above; entries with other names are ignored.
func KeysFromSecretData(data map[string][]byte) Keys {
	var keys Keys
	for k, v := range data {
		switch {
		case strings.HasSuffix(k, AgeKeySuffix):
			keys.Age = append(keys.Age, string(v))
		case strings.HasSuffix(k, PGPKeySuffix):
			keys.PGP = append(keys.PGP, string(v))
		}
	}
	return keys
}
