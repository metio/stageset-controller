// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actionplan"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/build"
	"github.com/metio/stageset-controller/internal/migrations"
)

// errInvalidVersion marks a terminal version-resolution failure (missing or
// unparseable version file / spec), as opposed to a transient fetch error.
var errInvalidVersion = errors.New("invalid version")

// migrationPlan is the version transition computed for one reconcile: the
// desired version, whether the controller is baselining (first adoption, no
// migrations), and the migrations to run, ordered by ascending target version.
type migrationPlan struct {
	versionSet bool
	desired    string
	baseline   bool
	pending    []*stagesv1.Migration
	// byStage maps a concrete stage name to the migrations anchored before it,
	// resolved from each migration's Stage value (anchor alias / name / empty).
	// Populated by resolveAnchors; a migration that resolves to no stage is a
	// terminal MigrationStageNotFound, never silently dropped.
	byStage map[string][]*stagesv1.Migration

	// sourceRevision and sourceDigest record the provenance of a ladder loaded
	// from spec.migrationsSourceRef — the resolved artifact's revision and digest
	// — so the execution events carry a self-contained audit trail of which
	// remote artifact a destructive migration came from. Empty for an inline
	// spec.migrations ladder (its content lives in the spec, and each executed
	// migration's content digest is recorded in status.executedMigrations).
	sourceRevision string
	sourceDigest   string
}

// pendingDetails builds the rich status preview of the pending migrations:
// boundary, the resolved anchor stage, action verbs, and content digest, so an
// operator (and the CLI) sees what will run without reading the spec — essential
// for a sourced ladder whose content is not in the spec.
func (p *migrationPlan) pendingDetails(ss *stagesv1.StageSet) []stagesv1.PendingMigration {
	if len(p.pending) == 0 {
		return nil
	}
	out := make([]stagesv1.PendingMigration, 0, len(p.pending))
	for _, m := range p.pending {
		var verbs []string
		for i := range m.Actions {
			verbs = append(verbs, m.Actions[i].Verb())
		}
		out = append(out, stagesv1.PendingMigration{
			Name:    m.Name,
			To:      m.To,
			From:    m.From,
			Stage:   anchorStage(ss, m.Stage),
			Actions: verbs,
			Digest:  migrationDigest(m),
		})
	}
	return out
}

func (p *migrationPlan) forStage(stage string) []*stagesv1.Migration {
	return p.byStage[stage]
}

// anchorStage resolves a migration's Stage value to a concrete stage name:
// empty anchors before the first stage; otherwise it matches the stage whose
// MigrationAnchor (preferred) or Name equals the value. Returns "" when nothing
// matches. Resolving by a stage-declared anchor role (not the literal stage
// name) is what lets one ladder be shared across StageSets named differently.
func anchorStage(ss *stagesv1.StageSet, anchor string) string {
	if anchor == "" {
		if len(ss.Spec.Stages) > 0 {
			return ss.Spec.Stages[0].Name
		}
		return ""
	}
	for i := range ss.Spec.Stages {
		if ss.Spec.Stages[i].MigrationAnchor == anchor {
			return ss.Spec.Stages[i].Name
		}
	}
	for i := range ss.Spec.Stages {
		if ss.Spec.Stages[i].Name == anchor {
			return ss.Spec.Stages[i].Name
		}
	}
	return ""
}

// resolveAnchors maps every pending migration to the stage it runs before,
// populating plan.byStage. A migration whose anchor resolves to no stage fails
// closed with MigrationStageNotFound rather than silently never running.
func resolveAnchors(ss *stagesv1.StageSet, plan *migrationPlan) (reason, msg string) {
	plan.byStage = make(map[string][]*stagesv1.Migration, len(ss.Spec.Stages))
	for _, m := range plan.pending {
		stage := anchorStage(ss, m.Stage)
		if stage == "" {
			return ReasonMigrationStageNotFound,
				fmt.Sprintf("migration %q anchors to %q, which matches no stage name or migrationAnchor", m.Name, m.Stage)
		}
		plan.byStage[stage] = append(plan.byStage[stage], m)
	}
	return "", ""
}

// planVersionMigrations resolves the desired version and the ordered migrations
// for the current transition. A nil reason means proceed; a non-empty reason is
// a terminal/transient Ready failure (InvalidVersion, DowngradeRequiresMigration)
// the caller surfaces. A non-nil error is a transient fetch failure (requeue).
func (r *StageSetReconciler) planVersionMigrations(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher) (*migrationPlan, string, string, error) {
	if ss.Spec.Version == nil {
		return &migrationPlan{versionSet: false}, "", "", nil
	}
	desired, err := r.resolveDesiredVersion(ctx, ss, resolved, fetcher)
	if err != nil {
		if errors.Is(err, errInvalidVersion) {
			return nil, ReasonInvalidVersion, err.Error(), nil
		}
		return nil, "", "", err // transient (fetch)
	}
	desiredV, err := semver.NewVersion(desired)
	if err != nil {
		return nil, ReasonInvalidVersion, fmt.Sprintf("desired version %q is not a semver: %v", desired, err), nil
	}
	plan := &migrationPlan{versionSet: true, desired: desired}

	current := ss.Status.Version
	if current == "" {
		// Baselining (Flyway-style): record the version, run no migrations.
		plan.baseline = true
		return plan, "", "", nil
	}
	currentV, err := semver.NewVersion(current)
	if err != nil {
		return nil, ReasonInvalidVersion, fmt.Sprintf("recorded status.version %q is not a semver: %v", current, err), nil
	}
	switch {
	case desiredV.LessThan(currentV):
		return nil, ReasonDowngradeRequiresMigration,
			fmt.Sprintf("desired version %s is below the deployed version %s; downgrades are refused", desired, current), nil
	case desiredV.Equal(currentV):
		return plan, "", "", nil // no transition
	}

	ladder, migSrc, lreason, lmsg, lerr := r.resolveMigrationLadder(ctx, ss, fetcher)
	if lerr != nil {
		return nil, "", "", lerr // transient (source not ready / fetch)
	}
	if lreason != "" {
		return nil, lreason, lmsg, nil
	}
	if migSrc != nil {
		plan.sourceRevision = migSrc.Revision
		plan.sourceDigest = migSrc.Digest
	}
	pending, perr := migrations.Select(ladder, currentV, desiredV)
	if perr != nil {
		return nil, ReasonInvalidVersion, perr.Error(), nil
	}
	if coverageGap(ss.Spec.Version.RequireMigrationCoverage, currentV, desiredV, len(pending)) {
		return nil, ReasonMigrationCoverageMissing,
			fmt.Sprintf("version crosses a major boundary %s → %s but no migration covers it, and requireMigrationCoverage is set", current, desired), nil
	}
	plan.pending = pending
	if reason, msg := resolveAnchors(ss, plan); reason != "" {
		return nil, reason, msg, nil
	}
	return plan, "", "", nil
}

// coverageGap reports whether requireMigrationCoverage should fail the
// transition: the flag is set, no migration is pending, and the transition
// crosses a major-version boundary (the case where advancing unmigrated is most
// likely a mistake).
func coverageGap(require bool, currentV, desiredV *semver.Version, pending int) bool {
	return require && pending == 0 && desiredV.Major() > currentV.Major()
}

// resolveMigrationLadder returns the migration ladder to plan against: the
// inline spec.migrations, or — when spec.migrationsSourceRef is set — the
// []Migration parsed from that source's artifact. A nil error with an empty
// reason means proceed; a non-empty reason is a terminal Ready failure
// (MigrationArtifactInvalid, RBACDenied, ResolveFailed); a non-nil error is a
// transient fetch/resolve failure the caller requeues on (fail closed — the
// version is not advanced while the ladder can't be loaded).
func (r *StageSetReconciler) resolveMigrationLadder(ctx context.Context, ss *stagesv1.StageSet, fetcher *artifact.Fetcher) (ladder []stagesv1.Migration, resolved *artifact.ResolvedArtifact, reason, msg string, err error) {
	src := ss.Spec.MigrationsSourceRef
	if src == nil {
		// Reconciler-side fallback for a bypassed or disabled admission webhook:
		// the inline ladder's names are the idempotency-ledger key, so a duplicate
		// name would let pruneSupersededLedger drop a sibling entry and re-run a
		// destructive migration. Reject it as a terminal spec error, mirroring the
		// ValidateLadder gate a sourced ladder gets below.
		if verr := migrations.ValidateLadder(ss.Spec.Migrations); verr != nil {
			return nil, nil, ReasonInvalidSpec, verr.Error(), nil
		}
		return ss.Spec.Migrations, nil, "", "", nil
	}
	// A migration source is always resolved same-namespace, independent of the
	// global --no-cross-namespace-refs: remote-authored destructive instructions
	// must not be pulled across a namespace boundary even where stage sources may
	// be. Admission rejects a cross-namespace migrationsSourceRef too; this is
	// the defense-in-depth fallback.
	resolver := &artifact.Resolver{NoCrossNamespace: true}
	ra, rerr := resolver.Resolve(ctx, r.Client, src.SourceRef, ss.Namespace)
	if rerr != nil {
		switch {
		case isPermanentAPIError(rerr):
			return nil, nil, ReasonRBACDenied, rbacDenialMessage("resolving the migrations source CR", rerr), nil
		case errors.Is(rerr, artifact.ErrSourceNotReady),
			errors.Is(rerr, artifact.ErrArtifactNotFound),
			errors.Is(rerr, artifact.ErrArtifactMissing):
			return nil, nil, "", "", fmt.Errorf("migrations source not ready: %w", rerr) // transient
		case errors.Is(rerr, artifact.ErrCrossNamespaceForbidden), errors.Is(rerr, artifact.ErrAmbiguousProducer):
			return nil, nil, ReasonResolveFailed, fmt.Sprintf("migrations source: %v", rerr), nil
		default:
			return nil, nil, "", "", rerr // transient API error
		}
	}
	if reason, msg := r.checkMigrationSourceVerified(ra); reason != "" {
		return nil, nil, reason, msg, nil
	}
	if reason, msg := r.checkMigrationSourcePinned(ss, ra); reason != "" {
		return nil, nil, reason, msg, nil
	}
	files, ferr := fetcher.Fetch(ctx, ra.URL, ra.Digest, src.Path)
	if ferr != nil {
		// A digest mismatch, SSRF rejection, or oversized/decompression-bomb
		// artifact would fail identically on every retry — surface it as terminal
		// MigrationArtifactInvalid rather than backing off forever. Genuinely
		// transient fetch failures (network, 5xx) still requeue.
		if terminalFetchError(ferr) {
			return nil, nil, ReasonMigrationArtifactInvalid, fmt.Sprintf("fetch migrations artifact: %v", ferr), nil
		}
		return nil, nil, "", "", fmt.Errorf("fetch migrations artifact: %w", ferr) // transient
	}
	ladder, perr := migrations.ParseLadder(files)
	if perr != nil {
		return nil, nil, ReasonMigrationArtifactInvalid, perr.Error(), nil
	}
	if verr := migrations.ValidateLadder(ladder); verr != nil {
		return nil, nil, ReasonMigrationArtifactInvalid, verr.Error(), nil
	}
	// A remote-authored http action with no host allowlist could reach any
	// in-cluster endpoint (the IP denylist deliberately permits private ranges
	// for in-cluster sources). Refuse a sourced ladder that uses http unless
	// --allowed-action-hosts scopes where those actions may connect.
	if len(r.AllowedActionHosts) == 0 && ladderHasHTTP(ladder) {
		return nil, nil, ReasonInvalidSpec,
			"the migration ladder sourced from spec.migrationsSourceRef uses an http action, but --allowed-action-hosts is not configured; remote-authored http actions require a host allowlist", nil
	}
	return ladder, &ra, "", "", nil
}

// ladderHasHTTP reports whether any migration in the ladder uses an http action.
func ladderHasHTTP(ladder []stagesv1.Migration) bool {
	for i := range ladder {
		for j := range ladder[i].Actions {
			if ladder[i].Actions[j].HTTP != nil {
				return true
			}
		}
	}
	return false
}

// checkMigrationSourceVerified gates a sourced ladder on signature provenance.
// A source whose verification FAILED (SourceVerified=False) is always refused;
// when --require-verified-migration-sources is set, a source that configures no
// verification at all (no SourceVerified condition) is refused too. Returns an
// empty reason to proceed. Fails closed — unverified destructive instructions
// are never executed.
func (r *StageSetReconciler) checkMigrationSourceVerified(ra artifact.ResolvedArtifact) (reason, msg string) {
	switch {
	case ra.Verified != nil && !*ra.Verified:
		return ReasonMigrationSourceNotVerified,
			"migrations source signature verification failed (SourceVerified=False); the source's spec.verify did not pass"
	case r.RequireVerifiedMigrationSources && ra.Verified == nil:
		return ReasonMigrationSourceNotVerified,
			"migrations source is not signature-verified and --require-verified-migration-sources is set; configure spec.verify (cosign/notation) on the source"
	}
	return "", ""
}

// checkMigrationSourcePinned gates a sourced ladder on revision immutability. A
// source pinned to a mutable tag/branch (not an OCIRepository digest or
// GitRepository commit) lets an upstream overwrite auto-roll new destructive
// content. With --require-pinned-migration-sources it is refused; otherwise it
// runs but emits a Warning so the auto-rollout risk is visible. A nil Pinned
// (pinning N/A — e.g. an in-cluster ExternalArtifact) is exempt.
func (r *StageSetReconciler) checkMigrationSourcePinned(ss *stagesv1.StageSet, ra artifact.ResolvedArtifact) (reason, msg string) {
	if ra.Pinned == nil || *ra.Pinned {
		return "", ""
	}
	if r.RequirePinnedMigrationSources {
		return ReasonMigrationSourceNotPinned,
			"migrations source is pinned to a mutable tag/branch, not an immutable digest/commit, and --require-pinned-migration-sources is set"
	}
	r.event(ss, corev1.EventTypeWarning, eventReasonMigrationSourceMutable,
		"migrations source is pinned to a mutable tag/branch; an upstream overwrite can auto-roll new destructive content — pin to a digest/commit")
	return "", ""
}

// resolveDesiredVersion reads the desired version from spec.version: an inline
// value, a field of a rendered object (fromObject), or a file inside a named
// stage's artifact (fromArtifact). A missing stage/file/object or an empty value
// is a terminal errInvalidVersion; a fetch failure is transient.
func (r *StageSetReconciler) resolveDesiredVersion(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher) (string, error) {
	v := ss.Spec.Version
	switch {
	case v.Value != "":
		return strings.TrimSpace(v.Value), nil
	case v.FromObject != nil:
		return r.versionFromObject(ctx, ss, resolved, fetcher, v.FromObject)
	case v.FromArtifact != nil:
		return r.versionFromArtifact(ctx, ss, resolved, fetcher, v.FromArtifact)
	default:
		return "", fmt.Errorf("%w: spec.version sets none of value, fromObject, or fromArtifact", errInvalidVersion)
	}
}

// stageIndex returns the index of the named stage, or -1.
func stageIndex(ss *stagesv1.StageSet, name string) int {
	for i := range ss.Spec.Stages {
		if ss.Spec.Stages[i].Name == name {
			return i
		}
	}
	return -1
}

// versionFromArtifact reads a bare semver string from a file in a stage's
// fetched artifact.
func (r *StageSetReconciler) versionFromArtifact(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher, ref *stagesv1.ArtifactVersionRef) (string, error) {
	idx := stageIndex(ss, ref.Stage)
	if idx < 0 {
		return "", fmt.Errorf("%w: spec.version.fromArtifact.stage %q is not a stage", errInvalidVersion, ref.Stage)
	}
	ra := resolved[idx]
	files, err := fetcher.Fetch(ctx, ra.URL, ra.Digest, "")
	if err != nil {
		return "", fmt.Errorf("fetch version artifact for stage %q: %w", ref.Stage, err)
	}
	content, ok := files[ref.Path]
	if !ok {
		return "", fmt.Errorf("%w: version file %q not found in stage %q artifact", errInvalidVersion, ref.Path, ref.Stage)
	}
	ver := strings.TrimSpace(content)
	if ver == "" {
		return "", fmt.Errorf("%w: version file %q is empty", errInvalidVersion, ref.Path)
	}
	return ver, nil
}

// versionFromObject builds a stage's manifests and reads the version from a
// field of one rendered object — by default the app.kubernetes.io/version
// label, so the version travels inside the manifests regardless of source kind.

func (r *StageSetReconciler) versionFromObject(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher, ref *stagesv1.ObjectVersionRef) (string, error) {
	idx, err := actionplan.VersionStageIndex(ss, ref.Stage)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errInvalidVersion, err)
	}
	stageName := ss.Spec.Stages[idx].Name
	ra := resolved[idx]
	files, err := fetcher.Fetch(ctx, ra.URL, ra.Digest, "")
	if err != nil {
		return "", fmt.Errorf("fetch version artifact for stage %q: %w", stageName, err)
	}
	stage := &ss.Spec.Stages[idx]
	vars, err := r.resolvePostBuildVars(ctx, ss, stage.PostBuild)
	if err != nil {
		return "", fmt.Errorf("resolve postBuild variables for version stage %q: %w", stageName, err)
	}
	objects, err := build.Build(files, build.Options{Path: stage.Path, Patches: stage.Patches}, vars)
	if err != nil {
		return "", fmt.Errorf("%w: building stage %q to read its version failed: %v", errInvalidVersion, stageName, err)
	}
	obj := actionplan.FindVersionObject(objects, ref)
	if obj == nil {
		return "", fmt.Errorf("%w: version object %s %q not found in stage %q manifests", errInvalidVersion, ref.Kind, ref.Name, stageName)
	}
	ver, err := actionplan.ExtractVersionField(obj, ref.FieldPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errInvalidVersion, err)
	}
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return "", fmt.Errorf("%w: version field on %s %q in stage %q resolved to empty", errInvalidVersion, ref.Kind, ref.Name, stageName)
	}
	return ver, nil
}

// actionExecutor runs a migration's actions, skipping those in done and calling
// record after each completes. *actions.Executor implements it; the seam lets
// tests drive the ledger logic without a cluster.
type actionExecutor interface {
	Run(ctx context.Context, namespace string, acts []stagesv1.Action, done map[string]bool, record func(name string) error) error
}

// migrationDigest is a stable content hash of a migration. The executed-ledger
// keys on (name, digest) so an edited sourced migration — same name, changed
// content — is treated as a new, unexecuted migration (the Flyway/Liquibase
// checksum rule) instead of being silently skipped.
func migrationDigest(m *stagesv1.Migration) string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// migrationKey is the ledger key for a migration: name@digest.
func migrationKey(m *stagesv1.Migration) string {
	return m.Name + "@" + migrationDigest(m)
}

// pruneSupersededLedger drops ledger entries for migration `name` whose content
// digest differs from keepMigKey — a re-authored migration supersedes its prior
// content, so the stale name@oldDigest entries (and their /action entries) are
// removed instead of accumulating in status. Migration names are unique within a
// ladder, so pruning by name is safe; the just-recorded keepMigKey is retained.
func pruneSupersededLedger(ss *stagesv1.StageSet, name, keepMigKey string) {
	ss.Status.ExecutedMigrations = dropSupersededLedger(ss.Status.ExecutedMigrations, name, keepMigKey, false)
	ss.Status.ExecutedMigrationActions = dropSupersededLedger(ss.Status.ExecutedMigrationActions, name, keepMigKey, true)
}

// dropSupersededLedger filters a ledger slice, removing entries for migration
// `name` whose migKey != keep. When action is true each entry is
// "<migKey>/<action>" and the migKey is the part before the first "/".
func dropSupersededLedger(entries []string, name, keep string, action bool) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		migKey := e
		if action {
			if before, _, ok := strings.Cut(e, "/"); ok {
				migKey = before
			}
		}
		if migEntryName(migKey) == name && migKey != keep {
			continue
		}
		out = append(out, e)
	}
	return out
}

// migEntryName returns the migration name from a "name@digest" migKey. Migration
// names are DNS-1123 (no "@"), so the name is the part before the first "@".
func migEntryName(migKey string) string {
	if before, _, ok := strings.Cut(migKey, "@"); ok {
		return before
	}
	return migKey
}

// actionsDoneFor returns the set of action names already completed for a
// migration key, read from the flat per-action ledger (entries "name@digest/action").
func actionsDoneFor(ledger []string, migKey string) map[string]bool {
	prefix := migKey + "/"
	done := make(map[string]bool)
	for _, e := range ledger {
		if after, ok := strings.CutPrefix(e, prefix); ok {
			done[after] = true
		}
	}
	return done
}

// runStageMigrations runs the pending migrations anchored to this stage, before
// the stage's own pre-actions. The migration ledger keys on (name, content
// digest): a fully-completed migration at the current content is skipped, and a
// content change re-runs it. Within a migration, each completed action is
// recorded in the per-action ledger so a retry of a partially-applied migration
// skips actions that already ran — destructive actions are never re-executed on
// retry. Both ledgers persist on the status (by failStage on failure, by the
// final status write on success) and are cleared once status.version advances.
func (r *StageSetReconciler) runStageMigrations(ctx context.Context, ss *stagesv1.StageSet, stage string, plan *migrationPlan, executor actionExecutor) error {
	if plan == nil || plan.baseline {
		return nil
	}
	doneMig := toStringSet(ss.Status.ExecutedMigrations)
	for _, m := range plan.forStage(stage) {
		migKey := migrationKey(m)
		if doneMig[migKey] {
			continue
		}
		actionsDone := actionsDoneFor(ss.Status.ExecutedMigrationActions, migKey)
		record := func(name string) error {
			ss.Status.ExecutedMigrationActions = append(ss.Status.ExecutedMigrationActions, migKey+"/"+name)
			return nil
		}
		src := migrationSourceSuffix(plan)
		r.event(ss, corev1.EventTypeNormal, eventReasonMigrationStarted,
			fmt.Sprintf("migration %q (to %s) starting%s", m.Name, m.To, src))
		if err := executor.Run(ctx, ss.Namespace, m.Actions, actionsDone, record); err != nil {
			r.event(ss, corev1.EventTypeWarning, eventReasonMigrationFailed,
				fmt.Sprintf("migration %q failed: %v", m.Name, err))
			return fmt.Errorf("migration %q: %w", m.Name, err)
		}
		ss.Status.ExecutedMigrations = append(ss.Status.ExecutedMigrations, migKey)
		// A re-authored migration (same name, new content → new digest) supersedes
		// its prior content; drop the stale name@oldDigest entries so the ledger
		// doesn't grow across a transition that never advances status.version.
		pruneSupersededLedger(ss, m.Name, migKey)
		doneMig[migKey] = true
		r.event(ss, corev1.EventTypeNormal, eventReasonMigrationCompleted,
			fmt.Sprintf("migration %q (to %s) completed%s", m.Name, m.To, src))
	}
	return nil
}

// migrationSourceSuffix renders the provenance of a sourced ladder for the
// execution events — the resolved artifact's revision and digest — so the audit
// record of a destructive migration names the exact remote artifact it came
// from. Empty for an inline spec.migrations ladder, whose per-migration content
// digests are already recorded in status.executedMigrations.
func migrationSourceSuffix(plan *migrationPlan) string {
	if plan == nil || plan.sourceDigest == "" {
		return ""
	}
	return fmt.Sprintf(" from source revision %s (artifact %s)", plan.sourceRevision, plan.sourceDigest)
}

// Event reasons for migration progress (Event-only; the Ready condition uses
// ReasonStageFailed on a failed migration).
const (
	eventReasonMigrationStarted       = "MigrationStarted"
	eventReasonMigrationCompleted     = "MigrationCompleted"
	eventReasonMigrationFailed        = "MigrationFailed"
	eventReasonMigrationSourceMutable = "MigrationSourceMutable"
	// eventReasonBaselined marks the first-adoption reconcile that records the
	// version without running migrations, so an operator can tell a baseline from
	// a real no-op and sanity-check the deployment is actually at that version.
	eventReasonBaselined = "MigrationsBaselined"
	// eventReasonActionSkipped marks a Version-scoped action held off a new
	// revision because its version episode already ran it — the config-churn
	// case the scope exists for.
	eventReasonActionSkipped = "ActionSkipped"
)
