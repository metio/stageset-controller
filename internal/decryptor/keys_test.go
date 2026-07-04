// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package decryptor

import "testing"

// KeysFromSecretData is the single mapping both the controller and the CLI
// use to read key Secrets; the suffix conventions must hold exactly.
func TestKeysFromSecretData(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		data             map[string][]byte
		wantAge, wantPGP int
	}{
		{"empty", nil, 0, 0},
		{"age key", map[string][]byte{"identity.agekey": []byte("AGE-SECRET-KEY-1X")}, 1, 0},
		{"pgp key", map[string][]byte{"owner.asc": []byte("-----BEGIN PGP")}, 0, 1},
		{"both plus noise", map[string][]byte{
			"a.agekey": []byte("k1"), "b.agekey": []byte("k2"),
			"c.asc":  []byte("p1"),
			"README": []byte("not a key"),
			"agekey": []byte("no dot, no match — the suffix includes the separator"),
		}, 2, 1},
		{"irrelevant entries only", map[string][]byte{"token": []byte("x"), "ca.crt": []byte("y")}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keys := KeysFromSecretData(tc.data)
			if len(keys.Age) != tc.wantAge || len(keys.PGP) != tc.wantPGP {
				t.Fatalf("got %d age / %d pgp keys, want %d / %d", len(keys.Age), len(keys.PGP), tc.wantAge, tc.wantPGP)
			}
		})
	}
}
