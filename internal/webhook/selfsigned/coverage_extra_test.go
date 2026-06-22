// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package selfsigned

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestBundle_WriteTo_CertWriteFails covers the second os.WriteFile branch in
// WriteTo: the dir exists (so tls.key writes), but tls.crt cannot be created
// because a directory already occupies that name.
func TestBundle_WriteTo_CertWriteFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tls.crt"), 0o755); err != nil {
		t.Fatalf("seed tls.crt dir: %v", err)
	}
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := b.WriteTo(dir); err == nil {
		t.Fatal("want error when tls.crt cannot be written")
	}
}

// TestBundle_WriteTo_KeyWriteFails covers the first os.WriteFile branch: tls.key
// itself cannot be created because a directory occupies that name.
func TestBundle_WriteTo_KeyWriteFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tls.key"), 0o755); err != nil {
		t.Fatalf("seed tls.key dir: %v", err)
	}
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := b.WriteTo(dir); err == nil {
		t.Fatal("want error when tls.key cannot be written")
	}
}

// TestRenewOnce_GuardDelayCtxCancel covers the ctx.Done() arm of the guard-delay
// select in renewOnce: cancelling the context during the guard window returns
// the context error before the trim patch is issued.
func TestRenewOnce_GuardDelayCtxCancel(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeVWCClient{vwc: makeVWC("vwc", 1, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Renewer{
		Input:      Input{Namespace: "stageset-system"},
		CertDir:    dir,
		VWCName:    "vwc",
		VWCClient:  fake,
		GuardDelay: time.Hour, // long enough that cancel always wins the select
	}
	// Cancel shortly after renewOnce enters the guard window.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := r.renewOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled from the guard window, got %v", err)
	}
	// The union patch (pre-guard) landed; the trim patch (post-guard) did not.
	if fake.updates != 1 {
		t.Errorf("want exactly the union patch issued, got %d updates", fake.updates)
	}
}

// TestRenewOnce_InjectedClockUsed pins the r.now injection seam: renewOnce reads
// its clock through r.now when set, so a fixed clock drives expiry pruning
// deterministically. With a clock far in the future, a pre-seeded still-dated CA
// is pruned as expired during the trim.
func TestRenewOnce_InjectedClockUsed(t *testing.T) {
	dir := t.TempDir()
	stale, err := Generate(Input{Namespace: "stageset-system", Validity: time.Hour})
	if err != nil {
		t.Fatalf("generate stale CA: %v", err)
	}
	fake := &fakeVWCClient{vwc: makeVWC("vwc", 1, stale.CABundle)}
	future := time.Now().Add(10 * 365 * 24 * time.Hour)
	r := &Renewer{
		Input:      Input{Namespace: "stageset-system"},
		CertDir:    dir,
		VWCName:    "vwc",
		VWCClient:  fake,
		GuardDelay: 1 * time.Millisecond,
		now:        func() time.Time { return future },
	}
	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("renewOnce: %v", err)
	}
	if bytes.Contains(fake.caBundle(), stale.CABundle) {
		t.Error("a CA expired relative to the injected clock must be pruned")
	}
}

// TestRenewer_Run_OnFailureFires covers the OnFailure callback arm of Run: a
// renew error invokes OnFailure with the error and the loop keeps ticking.
func TestRenewer_Run_OnFailureFires(t *testing.T) {
	dir := t.TempDir()
	var failures atomic.Int64
	r := &Renewer{
		Input:      Input{Namespace: "stageset-system"},
		CertDir:    dir,
		VWCName:    "vwc",
		VWCClient:  &alwaysErrUpdateClient{vwc: makeVWC("vwc", 1, nil)},
		Interval:   10 * time.Millisecond,
		GuardDelay: testGuardDelay,
		OnFailure:  func(error) { failures.Add(1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil after ctx cancel", err)
	}
	if failures.Load() == 0 {
		t.Error("OnFailure was never invoked despite persistent renew failures")
	}
}

// TestRenewer_Run_RecoversPanic covers the panic-recovery arm of Run: a VWC
// client that panics is turned into a renew error (surfaced via OnFailure)
// instead of crashing the loop, so future rotations still run.
func TestRenewer_Run_RecoversPanic(t *testing.T) {
	dir := t.TempDir()
	var failures atomic.Int64
	r := &Renewer{
		Input:      Input{Namespace: "stageset-system"},
		CertDir:    dir,
		VWCName:    "vwc",
		VWCClient:  &panicVWCClient{},
		Interval:   10 * time.Millisecond,
		GuardDelay: testGuardDelay,
		OnFailure:  func(error) { failures.Add(1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil after ctx cancel", err)
	}
	if failures.Load() == 0 {
		t.Error("a panicking tick must surface as a renew failure, not crash the loop")
	}
}

// TestRenewOnce_UnionPatchError covers renewOnce's first UpdateVWCCABundle error
// branch: a Get failure during the union patch aborts before WriteTo.
func TestRenewOnce_UnionPatchError(t *testing.T) {
	dir := t.TempDir()
	r := &Renewer{
		Input:      Input{Namespace: "stageset-system"},
		CertDir:    dir,
		VWCName:    "vwc",
		VWCClient:  &getErrVWCClient{},
		GuardDelay: testGuardDelay,
	}
	if err := r.renewOnce(context.Background()); err == nil {
		t.Fatal("want error when the union patch fails")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "tls.crt")); statErr == nil {
		t.Error("cert must not be written when the union patch fails")
	}
}

// TestRenewOnce_WriteCertError covers renewOnce's WriteTo error branch: the
// union patch succeeds, but a directory occupying tls.key fails the write.
func TestRenewOnce_WriteCertError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tls.key"), 0o755); err != nil {
		t.Fatalf("seed tls.key dir: %v", err)
	}
	fake := &fakeVWCClient{vwc: makeVWC("vwc", 1, nil)}
	r := &Renewer{
		Input:      Input{Namespace: "stageset-system"},
		CertDir:    dir,
		VWCName:    "vwc",
		VWCClient:  fake,
		GuardDelay: testGuardDelay,
	}
	if err := r.renewOnce(context.Background()); err == nil {
		t.Fatal("want error when WriteTo cannot write the cert")
	}
	// The union patch landed; the trim patch never ran (write failed first).
	if fake.updates != 1 {
		t.Errorf("want exactly the union patch before the write failure, got %d", fake.updates)
	}
}

// TestRenewOnce_TrimPatchError covers renewOnce's second UpdateVWCCABundle error
// branch: the union patch and cert write succeed, but the trim patch fails.
func TestRenewOnce_TrimPatchError(t *testing.T) {
	dir := t.TempDir()
	old, err := Generate(Input{Namespace: "stageset-system"})
	if err != nil {
		t.Fatalf("generate old CA: %v", err)
	}
	// Seed this pod's current CA so the trim genuinely removes a block and must
	// issue a second Update — which the client fails.
	c := &secondUpdateErrClient{vwc: makeVWC("vwc", 1, old.CABundle)}
	r := &Renewer{
		Input:      Input{Namespace: "stageset-system"},
		CertDir:    dir,
		VWCName:    "vwc",
		VWCClient:  c,
		GuardDelay: testGuardDelay,
		CurrentCA:  old.CABundle,
	}
	if err := r.renewOnce(context.Background()); err == nil {
		t.Fatal("want error when the trim patch fails")
	}
	// The cert was written (union succeeded, write succeeded) before the trim.
	if _, statErr := os.Stat(filepath.Join(dir, "tls.crt")); statErr != nil {
		t.Errorf("cert must be on disk before the trim patch: %v", statErr)
	}
}

// secondUpdateErrClient succeeds on the first Update (union) and fails on every
// Update after (trim), with a non-retriable error so UpdateVWCCABundle surfaces
// it immediately.
type secondUpdateErrClient struct {
	vwc   *admissionv1.ValidatingWebhookConfiguration
	calls int
}

func (s *secondUpdateErrClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	return s.vwc.DeepCopy(), nil
}

func (s *secondUpdateErrClient) Update(_ context.Context, vwc *admissionv1.ValidatingWebhookConfiguration, _ metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	s.calls++
	if s.calls >= 2 {
		return nil, errors.New("trim update failed")
	}
	s.vwc = vwc.DeepCopy()
	return s.vwc.DeepCopy(), nil
}

// TestPemCertBlocks_TrailingGarbageStops covers the rest-exhausted / nil-block
// break: a valid cert followed by non-PEM trailing bytes yields just the cert.
func TestPemCertBlocks_TrailingGarbageStops(t *testing.T) {
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	withGarbage := append(append([]byte{}, b.CABundle...), []byte("trailing non-pem bytes")...)
	if blocks := pemCertBlocks(withGarbage); len(blocks) != 1 {
		t.Fatalf("want 1 cert block before trailing garbage, got %d", len(blocks))
	}
}

// getErrVWCClient fails every Get so the renewer's union patch cannot start.
type getErrVWCClient struct{}

func (getErrVWCClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	return nil, errors.New("get failed")
}

func (getErrVWCClient) Update(_ context.Context, _ *admissionv1.ValidatingWebhookConfiguration, _ metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	return nil, errors.New("update failed")
}

// TestPemCertBlocks_SkipsNonCertificateBlocks covers the non-CERTIFICATE skip
// branch in pemCertBlocks: a private-key block interleaved with certs is
// dropped, certs are kept and canonically re-encoded.
func TestPemCertBlocks_SkipsNonCertificateBlocks(t *testing.T) {
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Sandwich the CA cert between two non-CERTIFICATE blocks.
	keyBlock := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("junk")})
	mixed := bytes.Join([][]byte{keyBlock, b.CABundle, keyBlock}, nil)

	blocks := pemCertBlocks(mixed)
	if len(blocks) != 1 {
		t.Fatalf("want exactly the one CERTIFICATE block, got %d", len(blocks))
	}
	if blk, _ := pem.Decode(blocks[0]); blk == nil || blk.Type != "CERTIFICATE" {
		t.Fatalf("returned block is not a CERTIFICATE: %v", blk)
	}
}

// TestPemCertBlocks_EmptyInput covers the empty-input loop guard.
func TestPemCertBlocks_EmptyInput(t *testing.T) {
	if blocks := pemCertBlocks(nil); len(blocks) != 0 {
		t.Fatalf("nil input must yield no blocks, got %d", len(blocks))
	}
}

// TestCombineCABundles_UnionsAndPrunes covers the bootstrap union helper: a
// still-valid existing CA is preserved alongside the new CA, while an expired CA
// already in the bundle is pruned.
func TestCombineCABundles_UnionsAndPrunes(t *testing.T) {
	valid, err := Generate(Input{Namespace: "x", Validity: time.Hour})
	if err != nil {
		t.Fatalf("generate valid: %v", err)
	}
	expired, err := Generate(Input{Namespace: "x", NotBefore: time.Now().Add(-48 * time.Hour), Validity: time.Hour})
	if err != nil {
		t.Fatalf("generate expired: %v", err)
	}
	fresh, err := Generate(Input{Namespace: "x", Validity: time.Hour})
	if err != nil {
		t.Fatalf("generate fresh: %v", err)
	}

	old := bytes.Join([][]byte{valid.CABundle, expired.CABundle}, nil)
	out := CombineCABundles(old, fresh.CABundle)

	if !bytes.Contains(out, valid.CABundle) {
		t.Error("still-valid existing CA must be preserved")
	}
	if !bytes.Contains(out, fresh.CABundle) {
		t.Error("new CA must be present after combine")
	}
	if bytes.Contains(out, expired.CABundle) {
		t.Error("expired CA must be pruned by combine")
	}
}

// TestCombineCABundles_NewOnly covers the single-replica bootstrap path: an empty
// existing bundle yields exactly the new CA.
func TestCombineCABundles_NewOnly(t *testing.T) {
	fresh, err := Generate(Input{Namespace: "x"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := CombineCABundles(nil, fresh.CABundle)
	if !bytes.Equal(out, bytes.Join(pemCertBlocks(fresh.CABundle), nil)) {
		t.Error("combining into an empty bundle must yield exactly the new CA")
	}
}

// alwaysErrUpdateClient fails every Update with a non-retriable error so the
// renewer's renew step fails on every tick.
type alwaysErrUpdateClient struct {
	vwc *admissionv1.ValidatingWebhookConfiguration
}

func (a *alwaysErrUpdateClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	return a.vwc.DeepCopy(), nil
}

func (a *alwaysErrUpdateClient) Update(_ context.Context, _ *admissionv1.ValidatingWebhookConfiguration, _ metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	return nil, errors.New("update always fails")
}

// panicVWCClient panics on Get so a renew tick panics, exercising Run's recovery.
type panicVWCClient struct{}

func (panicVWCClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	panic("boom in Get")
}

func (panicVWCClient) Update(_ context.Context, _ *admissionv1.ValidatingWebhookConfiguration, _ metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	panic("boom in Update")
}
