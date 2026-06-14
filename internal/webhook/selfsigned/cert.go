// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package selfsigned generates a CA + serving cert for the controller's
// validating webhook without depending on cert-manager. It pairs with
// UpdateVWCCABundle to inject the CA bundle into the
// ValidatingWebhookConfiguration so the apiserver trusts the controller's TLS
// handshake.
//
// The certs are valid for [Input.Validity] (default 1 year); the Renewer
// rotates them in-process. Clusters that prefer externally-managed material
// should use cert-manager instead (-webhook-cert-mode=cert-manager).
package selfsigned

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Input captures the per-cluster knobs Generate consumes. ServiceName and
// Namespace produce the standard DNS SANs an apiserver expects when dialing a
// Service-fronted webhook:
//
//	<service>.<namespace>.svc
//	<service>.<namespace>.svc.cluster.local
type Input struct {
	// ServiceName is the Service the webhook is reachable through. Defaults
	// to "stageset-controller-webhook" when empty.
	ServiceName string

	// Namespace is the Service's namespace. Required.
	Namespace string

	// Validity is how long the issued cert is valid for. Defaults to 365
	// days. The CA gets the same validity so the chain stays consistent.
	Validity time.Duration

	// NotBefore lets tests pin the cert's start time. Production passes the
	// zero value, which Generate fills from time.Now().
	NotBefore time.Time
}

// Bundle holds the PEM-encoded material a self-signed install needs: the CA
// bundle (for apiserver trust) and the serving cert+key (for the webhook
// HTTPS server).
type Bundle struct {
	// CABundle is the CA cert in PEM form, injected into every
	// ValidatingWebhookConfiguration.webhooks[*].clientConfig.caBundle.
	CABundle []byte

	// CertPEM is the serving cert; controller-runtime reads it from
	// <cert-dir>/tls.crt.
	CertPEM []byte

	// KeyPEM is the serving key; controller-runtime reads it from
	// <cert-dir>/tls.key.
	KeyPEM []byte

	// NotAfter is the serving cert's expiry.
	NotAfter time.Time
}

// Generate produces a new CA + serving cert pair signed by the CA. ECDSA
// P-256 keeps the bundle small without losing FIPS-acceptable security.
func Generate(in Input) (*Bundle, error) {
	if in.Namespace == "" {
		return nil, errors.New("selfsigned: Namespace is required")
	}
	if in.ServiceName == "" {
		in.ServiceName = "stageset-controller-webhook"
	}
	if in.Validity == 0 {
		in.Validity = 365 * 24 * time.Hour
	}
	notBefore := in.NotBefore
	if notBefore.IsZero() {
		notBefore = time.Now().Add(-1 * time.Minute) // small skew tolerance
	}
	notAfter := notBefore.Add(in.Validity)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: ca key: %w", err)
	}
	caSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "stageset-controller-webhook-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: sign ca: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: parse ca: %w", err)
	}

	servingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: serving key: %w", err)
	}
	servingSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	dnsNames := []string{
		in.ServiceName + "." + in.Namespace + ".svc",
		in.ServiceName + "." + in.Namespace + ".svc.cluster.local",
	}
	servingTmpl := &x509.Certificate{
		SerialNumber: servingSerial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	servingDER, err := x509.CreateCertificate(rand.Reader, servingTmpl, caCert, &servingKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: sign serving: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(servingKey)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: marshal serving key: %w", err)
	}

	return &Bundle{
		CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: servingDER}),
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		NotAfter: notAfter,
	}, nil
}

// WriteTo materializes tls.key then tls.crt under dir with 0o600 permissions —
// controller-runtime's webhook server reads these filenames. dir must exist.
//
// Key first, then cert: certwatcher's fsnotify hook fires on the cert file's
// write; by the time it reloads both files, the matching key is present.
// Writing the cert first would open a sub-millisecond window where certwatcher
// could pair the new cert with the old key and fail handshakes.
func (b *Bundle) WriteTo(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), b.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("selfsigned: write tls.key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), b.CertPEM, 0o600); err != nil {
		return fmt.Errorf("selfsigned: write tls.crt: %w", err)
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: serial: %w", err)
	}
	return n, nil
}
