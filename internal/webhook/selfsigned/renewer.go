// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package selfsigned

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// defaultGuardDelay is how long renewOnce waits between writing the new cert
// and trimming the caBundle back to just the new CA. The window must cover
// certwatcher's fsnotify reload and any in-flight admission requests using the
// old cert against the union-CA caBundle. 5s is generous on both.
const defaultGuardDelay = 5 * time.Second

// Renewer periodically regenerates the self-signed cert + key, writes them to
// CertDir, and re-patches the named VWC's caBundle so the apiserver keeps
// trusting the chain. Pairs with the one-shot bootstrap to give hot-reload
// rotation without cert-manager. controller-runtime's webhook server spawns
// certwatcher (fsnotify) when CertDir is set, so writing fresh files is enough
// to activate the new cert without a restart. One Renewer per process; stops
// cleanly on ctx cancel.
type Renewer struct {
	// Input is the same shape Generate consumes — passed through on every
	// rotation so SANs / validity stay stable.
	Input Input

	// CertDir is where tls.crt and tls.key are written. Must match the
	// controller-runtime webhook server's CertDir.
	CertDir string

	// VWCName is the ValidatingWebhookConfiguration whose caBundle is
	// re-patched on each rotation.
	VWCName string

	// VWCClient patches the VWC. Tests substitute a fake.
	VWCClient VWCClient

	// Interval is how often the renewer fires. Zero defaults to
	// Input.Validity / 3.
	Interval time.Duration

	// GuardDelay is how long renewOnce waits between writing the new cert/key
	// and trimming the VWC caBundle back to just the new CA. During this
	// window the caBundle holds both old and new CA, so any admission request
	// verifies. Zero defaults to defaultGuardDelay.
	GuardDelay time.Duration

	// CurrentCA is this pod's CA PEM block — the one it currently serves and
	// whose trust it manages in the VWC caBundle. The trim step removes
	// exactly this block (the pod's previous CA), leaving every other
	// replica's CA untouched, so a rotation never evicts a peer.
	CurrentCA []byte

	// Logger receives renewal events. nil falls back to slog.Default.
	Logger *slog.Logger

	// now supplies the wall-clock used to prune expired CA blocks. nil falls
	// back to time.Now; tests inject a fixed clock.
	now func() time.Time

	// OnFailure, if non-nil, fires after every renewOnce error with the error
	// as argument — wired to a metric so background failures are observable.
	OnFailure func(error)
}

// Run blocks until ctx is canceled, regenerating + republishing the cert at
// every Interval tick. Returns nil on cancel so SIGTERM is a clean shutdown.
// Run does NOT generate the initial cert — bootstrap once before mgr.Start,
// then hand off to Run.
func (r *Renewer) Run(ctx context.Context) error {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if r.CertDir == "" {
		return errors.New("renewer: CertDir is required")
	}
	if r.VWCName == "" {
		return errors.New("renewer: VWCName is required")
	}
	if r.VWCClient == nil {
		return errors.New("renewer: VWCClient is required")
	}

	interval := r.Interval
	if interval == 0 {
		validity := r.Input.Validity
		if validity == 0 {
			validity = 365 * 24 * time.Hour
		}
		interval = validity / 3
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Recover a panic so one bad tick can't silently stop all future
			// rotation (admission would then break cluster-wide at expiry with
			// no prior signal). A recovered panic is treated as a renew error.
			err := func() (err error) {
				defer func() {
					if p := recover(); p != nil {
						err = fmt.Errorf("renewer: panic in renewOnce: %v", p)
					}
				}()
				return r.renewOnce(ctx)
			}()
			if err != nil {
				logger.WarnContext(ctx, "Self-signed webhook cert renewal failed", slog.Any("error", err))
				if r.OnFailure != nil {
					r.OnFailure(err)
				}
				continue
			}
			logger.InfoContext(ctx, "Self-signed webhook cert renewed",
				slog.String("certDir", r.CertDir), slog.String("vwc", r.VWCName))
		}
	}
}

// renewOnce is the per-tick work — extracted so a test can drive one rotation.
//
// Dual-CA, peer-preserving rotation:
//  1. Generate the new (cert, key, CA).
//  2. Patch the caBundle to (current ∪ NewCA), expired blocks pruned. The
//     webhook is still serving OldCert.
//  3. WriteTo writes the new cert+key; certwatcher reloads to NewCert.
//  4. Sleep GuardDelay so in-flight OldCert requests finish under union trust.
//  5. Re-read and patch to (current − thisPodOldCA + NewCA). Only this pod's
//     superseded CA is dropped; peers' CAs stay trusted.
func (r *Renewer) renewOnce(ctx context.Context) error {
	bundle, err := Generate(r.Input)
	if err != nil {
		return fmt.Errorf("renewer: generate: %w", err)
	}

	guard := r.GuardDelay
	if guard <= 0 {
		guard = defaultGuardDelay
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}

	if err := UpdateVWCCABundle(ctx, r.VWCClient, r.VWCName, func(cur []byte) []byte {
		return mergeCABundle(cur, bundle.CABundle, nil, now())
	}); err != nil {
		return fmt.Errorf("renewer: union: %w", err)
	}
	if err := bundle.WriteTo(r.CertDir); err != nil {
		return fmt.Errorf("renewer: write cert: %w", err)
	}
	select {
	case <-time.After(guard):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := UpdateVWCCABundle(ctx, r.VWCClient, r.VWCName, func(cur []byte) []byte {
		return mergeCABundle(cur, bundle.CABundle, r.CurrentCA, now())
	}); err != nil {
		return fmt.Errorf("renewer: trim: %w", err)
	}
	r.CurrentCA = bundle.CABundle
	return nil
}

// mergeCABundle returns a re-encoded CA bundle: every CERTIFICATE block in
// current is preserved EXCEPT expired ones or those equal to a block in remove
// (this pod's superseded CA); add is then ensured present. Blocks are
// deduplicated and canonically encoded. Preserving foreign blocks keeps a
// multi-replica rotation from evicting a peer; pruning expired blocks keeps the
// bundle bounded.
func mergeCABundle(current, add, remove []byte, now time.Time) []byte {
	removeSet := map[string]bool{}
	for _, enc := range pemCertBlocks(remove) {
		removeSet[string(enc)] = true
	}
	seen := map[string]bool{}
	var out [][]byte
	for _, enc := range pemCertBlocks(current) {
		k := string(enc)
		if seen[k] || removeSet[k] {
			continue
		}
		if certExpired(enc, now) {
			continue
		}
		seen[k] = true
		out = append(out, enc)
	}
	for _, enc := range pemCertBlocks(add) {
		k := string(enc)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, enc)
	}
	return bytes.Join(out, nil)
}

// pemCertBlocks decodes data into its CERTIFICATE PEM blocks, each re-encoded
// canonically so equal certs compare byte-for-byte. Non-CERTIFICATE blocks and
// trailing garbage are skipped.
func pemCertBlocks(data []byte) [][]byte {
	var blocks [][]byte
	rest := data
	for len(rest) > 0 {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		blocks = append(blocks, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: blk.Bytes}))
	}
	return blocks
}

// certExpired reports whether the single CERTIFICATE PEM block has expired as
// of now. An unparseable block is treated as NOT expired — dropping a CA we
// can't read would be more dangerous than keeping it.
func certExpired(enc []byte, now time.Time) bool {
	blk, _ := pem.Decode(enc)
	if blk == nil {
		return false
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return false
	}
	return !cert.NotAfter.After(now)
}

// CombineCABundles unions newCA into oldCA for the bootstrap path: it keeps
// every still-valid CA already present (so peers stay trusted across a rollout
// / chart re-install), prunes expired blocks, and ensures newCA is present.
func CombineCABundles(oldCA, newCA []byte) []byte {
	return mergeCABundle(oldCA, newCA, nil, time.Now())
}
