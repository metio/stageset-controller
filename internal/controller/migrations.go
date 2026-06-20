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
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/jsonpath"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/build"
	"github.com/metio/stageset-controller/internal/migrations"
)

// versionLabel is the Kubernetes-recommended label carrying an application's
// version. It is the default field spec.version.fromObject reads.
const versionLabel = "app.kubernetes.io/version"

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

	ladder, lreason, lmsg, lerr := r.resolveMigrationLadder(ctx, ss, fetcher)
	if lerr != nil {
		return nil, "", "", lerr // transient (source not ready / fetch)
	}
	if lreason != "" {
		return nil, lreason, lmsg, nil
	}
	pending, perr := selectMigrations(ladder, currentV, desiredV)
	if perr != nil {
		return nil, ReasonInvalidVersion, perr.Error(), nil
	}
	plan.pending = pending
	if reason, msg := resolveAnchors(ss, plan); reason != "" {
		return nil, reason, msg, nil
	}
	return plan, "", "", nil
}

// resolveMigrationLadder returns the migration ladder to plan against: the
// inline spec.migrations, or — when spec.migrationsSourceRef is set — the
// []Migration parsed from that source's artifact. A nil error with an empty
// reason means proceed; a non-empty reason is a terminal Ready failure
// (MigrationArtifactInvalid, RBACDenied, ResolveFailed); a non-nil error is a
// transient fetch/resolve failure the caller requeues on (fail closed — the
// version is not advanced while the ladder can't be loaded).
func (r *StageSetReconciler) resolveMigrationLadder(ctx context.Context, ss *stagesv1.StageSet, fetcher *artifact.Fetcher) (ladder []stagesv1.Migration, reason, msg string, err error) {
	src := ss.Spec.MigrationsSourceRef
	if src == nil {
		return ss.Spec.Migrations, "", "", nil
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
			return nil, ReasonRBACDenied, rbacDenialMessage("resolving the migrations source CR", rerr), nil
		case errors.Is(rerr, artifact.ErrSourceNotReady),
			errors.Is(rerr, artifact.ErrArtifactNotFound),
			errors.Is(rerr, artifact.ErrArtifactMissing):
			return nil, "", "", fmt.Errorf("migrations source not ready: %w", rerr) // transient
		case errors.Is(rerr, artifact.ErrCrossNamespaceForbidden), errors.Is(rerr, artifact.ErrAmbiguousProducer):
			return nil, ReasonResolveFailed, fmt.Sprintf("migrations source: %v", rerr), nil
		default:
			return nil, "", "", rerr // transient API error
		}
	}
	if reason, msg := r.checkMigrationSourceVerified(ra); reason != "" {
		return nil, reason, msg, nil
	}
	if reason, msg := r.checkMigrationSourcePinned(ss, ra); reason != "" {
		return nil, reason, msg, nil
	}
	files, ferr := fetcher.Fetch(ctx, ra.URL, ra.Digest, src.Path)
	if ferr != nil {
		// A digest mismatch, SSRF rejection, or oversized/decompression-bomb
		// artifact would fail identically on every retry — surface it as terminal
		// MigrationArtifactInvalid rather than backing off forever. Genuinely
		// transient fetch failures (network, 5xx) still requeue.
		if terminalFetchError(ferr) {
			return nil, ReasonMigrationArtifactInvalid, fmt.Sprintf("fetch migrations artifact: %v", ferr), nil
		}
		return nil, "", "", fmt.Errorf("fetch migrations artifact: %w", ferr) // transient
	}
	ladder, perr := migrations.ParseLadder(files)
	if perr != nil {
		return nil, ReasonMigrationArtifactInvalid, perr.Error(), nil
	}
	if verr := migrations.ValidateLadder(ladder); verr != nil {
		return nil, ReasonMigrationArtifactInvalid, verr.Error(), nil
	}
	// A remote-authored http action with no host allowlist could reach any
	// in-cluster endpoint (the IP denylist deliberately permits private ranges
	// for in-cluster sources). Refuse a sourced ladder that uses http unless
	// --allowed-action-hosts scopes where those actions may connect.
	if len(r.AllowedActionHosts) == 0 && ladderHasHTTP(ladder) {
		return nil, ReasonInvalidSpec,
			"the migration ladder sourced from spec.migrationsSourceRef uses an http action, but --allowed-action-hosts is not configured; remote-authored http actions require a host allowlist", nil
	}
	return ladder, "", "", nil
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

// selectMigrations returns the migrations whose target version is crossed by
// current -> desired (current < to <= desired) and whose optional `from`
// constraint the current version satisfies, ordered by ascending target.
func selectMigrations(migrations []stagesv1.Migration, currentV, desiredV *semver.Version) ([]*stagesv1.Migration, error) {
	type scored struct {
		m  *stagesv1.Migration
		to *semver.Version
	}
	var picked []scored
	for i := range migrations {
		m := &migrations[i]
		toV, err := semver.NewVersion(m.To)
		if err != nil {
			return nil, fmt.Errorf("%w: migration %q has invalid to %q", errInvalidVersion, m.Name, m.To)
		}
		if !toV.GreaterThan(currentV) || toV.GreaterThan(desiredV) {
			continue // boundary not crossed by this transition
		}
		if m.From != "" {
			constraint, err := semver.NewConstraint(m.From)
			if err != nil {
				return nil, fmt.Errorf("%w: migration %q has invalid from %q", errInvalidVersion, m.Name, m.From)
			}
			if !constraint.Check(currentV) {
				continue // current version does not satisfy the from constraint
			}
		}
		picked = append(picked, scored{m: m, to: toV})
	}
	// Ascending target version; equal targets keep spec order (stable sort).
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].to.LessThan(picked[j].to) })
	out := make([]*stagesv1.Migration, 0, len(picked))
	for _, s := range picked {
		out = append(out, s.m)
	}
	return out, nil
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
	idx := stageIndex(ss, ref.Stage)
	if idx < 0 {
		return "", fmt.Errorf("%w: spec.version.fromObject.stage %q is not a stage", errInvalidVersion, ref.Stage)
	}
	ra := resolved[idx]
	files, err := fetcher.Fetch(ctx, ra.URL, ra.Digest, "")
	if err != nil {
		return "", fmt.Errorf("fetch version artifact for stage %q: %w", ref.Stage, err)
	}
	stage := &ss.Spec.Stages[idx]
	vars, err := r.resolvePostBuildVars(ctx, ss.Namespace, stage.PostBuild)
	if err != nil {
		return "", fmt.Errorf("resolve postBuild variables for version stage %q: %w", ref.Stage, err)
	}
	objects, err := build.Build(files, build.Options{Path: stage.Path, Patches: stage.Patches}, vars)
	if err != nil {
		return "", fmt.Errorf("%w: building stage %q to read its version failed: %v", errInvalidVersion, ref.Stage, err)
	}
	obj := findVersionObject(objects, ref)
	if obj == nil {
		return "", fmt.Errorf("%w: version object %s %q not found in stage %q manifests", errInvalidVersion, ref.Kind, ref.Name, ref.Stage)
	}
	ver, err := extractVersionField(obj, ref.FieldPath)
	if err != nil {
		return "", err
	}
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return "", fmt.Errorf("%w: version field on %s %q in stage %q resolved to empty", errInvalidVersion, ref.Kind, ref.Name, ref.Stage)
	}
	return ver, nil
}

// findVersionObject returns the rendered object matching the ref's Kind and
// Name (and APIVersion when set), or nil.
func findVersionObject(objects []*unstructured.Unstructured, ref *stagesv1.ObjectVersionRef) *unstructured.Unstructured {
	for _, o := range objects {
		if o.GetKind() != ref.Kind || o.GetName() != ref.Name {
			continue
		}
		if ref.APIVersion != "" && o.GetAPIVersion() != ref.APIVersion {
			continue
		}
		return o
	}
	return nil
}

// extractVersionField reads the version string from an object. An empty
// fieldPath reads the app.kubernetes.io/version label; otherwise fieldPath is a
// kubectl-style JSONPath that must resolve to the bare version string.
func extractVersionField(obj *unstructured.Unstructured, fieldPath string) (string, error) {
	if fieldPath == "" {
		val, found, err := unstructured.NestedString(obj.Object, "metadata", "labels", versionLabel)
		if err != nil || !found {
			return "", fmt.Errorf("%w: %s %q has no %s label; set spec.version.fromObject.fieldPath to read the version from a different field",
				errInvalidVersion, obj.GetKind(), obj.GetName(), versionLabel)
		}
		return val, nil
	}
	jp := jsonpath.New("version").AllowMissingKeys(false)
	if err := jp.Parse(fieldPath); err != nil {
		return "", fmt.Errorf("%w: spec.version.fromObject.fieldPath %q is not valid JSONPath: %v", errInvalidVersion, fieldPath, err)
	}
	var buf strings.Builder
	if err := jp.Execute(&buf, obj.Object); err != nil {
		return "", fmt.Errorf("%w: evaluating fieldPath %q on %s %q: %v", errInvalidVersion, fieldPath, obj.GetKind(), obj.GetName(), err)
	}
	return buf.String(), nil
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

// actionsDoneFor returns the set of action names already completed for a
// migration key, read from the flat per-action ledger (entries "name@digest/action").
func actionsDoneFor(ledger []string, migKey string) map[string]bool {
	prefix := migKey + "/"
	done := make(map[string]bool)
	for _, e := range ledger {
		if strings.HasPrefix(e, prefix) {
			done[strings.TrimPrefix(e, prefix)] = true
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
		r.event(ss, corev1.EventTypeNormal, eventReasonMigrationStarted,
			fmt.Sprintf("migration %q (to %s) starting", m.Name, m.To))
		if err := executor.Run(ctx, ss.Namespace, m.Actions, actionsDone, record); err != nil {
			r.event(ss, corev1.EventTypeWarning, eventReasonMigrationFailed,
				fmt.Sprintf("migration %q failed: %v", m.Name, err))
			return fmt.Errorf("migration %q: %w", m.Name, err)
		}
		ss.Status.ExecutedMigrations = append(ss.Status.ExecutedMigrations, migKey)
		doneMig[migKey] = true
		r.event(ss, corev1.EventTypeNormal, eventReasonMigrationCompleted,
			fmt.Sprintf("migration %q (to %s) completed", m.Name, m.To))
	}
	return nil
}

// Event reasons for migration progress (Event-only; the Ready condition uses
// ReasonStageFailed on a failed migration).
const (
	eventReasonMigrationStarted       = "MigrationStarted"
	eventReasonMigrationCompleted     = "MigrationCompleted"
	eventReasonMigrationFailed        = "MigrationFailed"
	eventReasonMigrationSourceMutable = "MigrationSourceMutable"
)
