// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/metio/stageset-controller/internal/metrics"
	"github.com/metio/stageset-controller/internal/opstate"
)

const (
	// superviseBaseDelay is the wait after the first failed manager start.
	superviseBaseDelay = time.Second
	// superviseMaxDelay caps the exponential growth. A controller whose RBAC or
	// NetworkPolicy is fixed out of band recovers within this long at worst,
	// which is the number to weigh against the request rate a wedged pod puts
	// on an apiserver that may itself be the thing recovering.
	superviseMaxDelay = 5 * time.Minute
	// superviseJitterFraction spreads retries across replicas so a fleet that
	// lost the apiserver together does not reconnect in lockstep.
	superviseJitterFraction = 0.2
	// superviseHealthyRun is how long a manager has to last for its failure to
	// count as a fresh cause rather than a continuing one, which is what resets
	// the backoff. Availability alone is the wrong test: a manager whose cache
	// syncs and which then dies on the next step — a webhook server with no
	// certificate yet, say — would reset the delay on every attempt and retry
	// about once a second for as long as the cause lasted.
	superviseHealthyRun = time.Minute
)

// supervise starts the manager and restarts it for as long as ctx lives,
// reporting each transition through state.
//
// Everything the manager needs from the apiserver before it can serve — the
// discovery behind each reconciler's watches, the RESTMapper lookups the
// producer watches are gated on — happens while it is being built, so an
// apiserver that is unreachable, or an RBAC grant that is missing, fails the
// build outright. Treating that as fatal ends the process, and the kubelet then
// restarts the pod into the same failure for as long as the cause lasts, which
// buries the reason under restart churn and takes the probe and metrics
// endpoints down with it. Retrying in place keeps those endpoints answering, so
// the cause stays readable, and the manager comes up on its own once it is
// cleared.
//
// A lost leader-election lease arrives here the same way, as mgr.Start
// returning: the next attempt blocks on acquiring the lease again, which is the
// behaviour the lease exists for.
func supervise(
	ctx context.Context,
	state *opstate.State,
	logger *slog.Logger,
	run func(context.Context) error,
	delayFor func(attempt int) time.Duration,
	healthyRun time.Duration,
) {
	for attempt := 1; ; attempt++ {
		state.RecordAttempt()
		started := time.Now()
		errCh := make(chan error, 1)
		go func() { errCh <- run(ctx) }()

		var err error
		select {
		case err = <-errCh:
		case <-ctx.Done():
			// A manager that reached a synced cache is draining its in-flight
			// reconciles, so it is awaited — that window is the point of a
			// graceful shutdown, and the manager's own GracefulShutdownTimeout
			// bounds it. A manager still being built has nothing to drain, and
			// it can be stuck in an apiserver call that ignores the context:
			// client-go's discovery honours the REST client's own timeout rather
			// than ours, so against an unreachable apiserver the build takes
			// about ten seconds to fail. Waiting that out would add it to every
			// pod deletion in a degraded cluster, so the orphan is left to
			// finish on its own.
			if !state.Available() {
				return
			}
			err = <-errCh
		}
		if ctx.Err() != nil {
			// Shutdown, not failure: the context that stopped the manager is
			// the one the process is exiting on.
			return
		}
		metrics.ManagerStartFailuresTotal.Inc()
		reason := "manager stopped"
		if err != nil && !errors.Is(err, context.Canceled) {
			reason = err.Error()
		}
		state.MarkUnavailable(reason)
		// A manager that ran for a while starts its backoff over: the cause is
		// fresh, and the delay that had grown during an earlier outage says
		// nothing about this one. One that failed quickly keeps the growing
		// delay, whether it got as far as syncing or not.
		if time.Since(started) >= healthyRun {
			attempt = 1
		}
		delay := delayFor(attempt)
		// The backoff is what keeps this from flooding: the retries thin out to
		// one every superviseMaxDelay, and each carries the reason, so a wedged
		// controller stays visible without a rate limiter of its own.
		if attempt == 1 {
			logger.Error("manager unavailable, restarting", "error", errors.New(reason), "retryIn", delay)
		} else {
			logger.Warn("manager still unavailable, restarting", "reason", reason, "attempt", attempt, "retryIn", delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// superviseDelay is the wait before the attempt-th restart: exponential from
// superviseBaseDelay, capped at superviseMaxDelay, spread by a jitter fraction.
func superviseDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Doubling in a loop that stops at the cap keeps the growth away from the
	// range where a shift would overflow, whatever the attempt count reaches.
	delay := superviseBaseDelay
	for i := 1; i < attempt && delay < superviseMaxDelay; i++ {
		delay *= 2
	}
	delay = min(delay, superviseMaxDelay)
	spread := float64(delay) * superviseJitterFraction
	// #nosec G404 -- spreading retries across replicas, not a security decision.
	return time.Duration(float64(delay) - spread + rand.Float64()*2*spread)
}
