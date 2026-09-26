// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// apiserverGate is a TCP proxy in front of the envtest apiserver whose
// reachability a test can switch at will. Closed, it accepts a connection and
// drops it, which is what an apiserver behind a NetworkPolicy that admits no
// egress looks like from the pod. Open, it forwards, so the same process sees
// the apiserver appear without being restarted.
//
// It proxies TCP rather than terminating TLS, so the client's handshake still
// runs against the real apiserver certificate. Only the port changes, and the
// envtest serving certificate covers 127.0.0.1, so verification is unaffected.
// Mirrors jaas's apiserverGate.
type apiserverGate struct {
	listener net.Listener
	target   string
	open     atomic.Bool
	wg       sync.WaitGroup

	// Forwarded connections are tracked so teardown can close them. A watch
	// that the client leaves in its idle pool keeps its copy loops blocked for
	// as long as the peer holds the socket open, and waiting that out would add
	// a minute and a half to the test.
	mu    sync.Mutex
	conns []net.Conn
}

func (g *apiserverGate) track(conns ...net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conns = append(g.conns, conns...)
}

func (g *apiserverGate) closeTracked() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.conns {
		_ = c.Close()
	}
	g.conns = nil
}

func newAPIServerGate(t *testing.T, target string) *apiserverGate {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gate listen: %v", err)
	}
	g := &apiserverGate{listener: l, target: target}
	g.wg.Go(func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			if !g.open.Load() {
				_ = conn.Close()
				continue
			}
			g.wg.Go(func() { g.forward(conn) })
		}
	})
	t.Cleanup(func() {
		_ = l.Close()
		g.closeTracked()
		g.wg.Wait()
	})
	return g
}

func (g *apiserverGate) forward(client net.Conn) {
	defer func() { _ = client.Close() }()
	upstream, err := net.DialTimeout("tcp", g.target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	g.track(client, upstream)
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.Copy(upstream, client) })
	wg.Go(func() { _, _ = io.Copy(client, upstream) })
	wg.Wait()
}

func (g *apiserverGate) addr() string { return g.listener.Addr().String() }

// envtestGatedSetup boots envtest and points KUBECONFIG at a kubeconfig that
// reaches it only through the returned gate.
func envtestGatedSetup(t *testing.T) *apiserverGate {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets unavailable (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{mainCRDDir(t)},
		ErrorIfCRDPathMissing: true,
	}
	if _, err := env.Start(); err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	user, err := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("envtest AddUser: %v", err)
	}
	raw, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("envtest KubeConfig: %v", err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatalf("parse kubeconfig: %v", err)
	}
	if len(cfg.Clusters) != 1 {
		t.Fatalf("kubeconfig has %d clusters, want 1", len(cfg.Clusters))
	}
	var gate *apiserverGate
	for _, cluster := range cfg.Clusters {
		gate = newAPIServerGate(t, strings.TrimPrefix(cluster.Server, "https://"))
		cluster.Server = "https://" + gate.addr()
	}
	rewritten, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatalf("write kubeconfig file: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
	return gate
}

// awaitProbe polls path until it answers with want. The failure names the last
// body seen, which for /manager is the reason the manager is down.
func awaitProbe(t *testing.T, addr, path string, want int, timeout time.Duration) {
	t.Helper()
	started := time.Now()
	deadline := started.Add(timeout)
	last, body := -1, ""
	for time.Now().Before(deadline) {
		last, body = probeGet(t, addr, path)
		if last == want {
			// The wait is logged because these transitions are paced by the
			// supervisor's backoff, which is the interesting number when this
			// test turns slow.
			t.Logf("GET %s returned %d after %v", path, want, time.Since(started).Round(time.Millisecond))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if path != "/manager" {
		// The manager's own status carries the reason behind every other
		// endpoint's answer, so a failure anywhere else reports it too.
		_, status := probeGet(t, addr, "/manager")
		t.Fatalf("GET %s never returned %d within %v (last %d: %s; /manager: %s)", path, want, timeout, last, body, status)
	}
	t.Fatalf("GET %s never returned %d within %v (last %d: %s)", path, want, timeout, last, body)
}

// TestRun_DegradesWhileTheApiserverIsUnreachable covers both transitions the
// degraded controller has to survive.
//
// While the apiserver cannot be reached, the process keeps running: exiting
// would hand the kubelet a restart loop for as long as the cause lasts, and the
// probe and metrics endpoints — the only places the cause is legible — would go
// with it. /healthz stays green so nothing restarts the pod, /readyz reports
// not-ready so it stays out of its Services, /manager carries the reason, and
// the availability gauge is scrapeable while it reads 0.
//
// Once the apiserver becomes reachable, the manager comes up in the same
// process — no restart — and every signal flips.
func TestRun_DegradesWhileTheApiserverIsUnreachable(t *testing.T) {
	gate := envtestGatedSetup(t)

	probeAddr := "127.0.0.1:" + freePort(t)
	metricsAddr := "127.0.0.1:" + freePort(t)
	args := []string{
		"-metrics-bind-address=" + metricsAddr,
		"-health-probe-bind-address=" + probeAddr,
		"-gate-bind-address=",
		"-leader-elect=false",
		// The webhook is on by default and its server needs TLS material the
		// test does not provision; off, the test isolates the one variable it is
		// about — whether the apiserver can be reached.
		"-enable-webhook=false",
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, args, nil, io.Discard) }()

	awaitProbe(t, probeAddr, "/manager", http.StatusServiceUnavailable, 60*time.Second)

	if code, _ := probeGet(t, probeAddr, "/healthz"); code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d while the manager is down", code, http.StatusOK)
	}
	if code, _ := probeGet(t, probeAddr, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz = %d, want %d while the manager is down", code, http.StatusServiceUnavailable)
	}
	if _, body := probeGet(t, metricsAddr, "/metrics"); !strings.Contains(body, "stageset_manager_available 0") {
		t.Errorf("metrics do not report stageset_manager_available 0")
	}

	// The process is still the one that started: nothing restarted it, and it
	// has not exited.
	select {
	case code := <-done:
		t.Fatalf("run returned %d while the apiserver was unreachable; want it still serving", code)
	default:
	}

	// Now let the apiserver through.
	gate.open.Store(true)
	awaitProbe(t, probeAddr, "/manager", http.StatusOK, 90*time.Second)
	awaitProbe(t, probeAddr, "/readyz", http.StatusOK, 30*time.Second)
	if _, body := probeGet(t, metricsAddr, "/metrics"); !strings.Contains(body, "stageset_manager_available 1") {
		t.Errorf("metrics do not report stageset_manager_available 1 after recovery")
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("run exit code = %d, want 0", code)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("run did not return within 60s of context cancellation")
	}
}
