// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	fluxconditions "github.com/fluxcd/pkg/runtime/conditions"
	fluxpatch "github.com/fluxcd/pkg/runtime/patch"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/metricsource"
)

// Fleet-rollout Ready-condition reasons. These are FleetRollout's own condition
// vocabulary, separate from the StageSet Reason* set.
const (
	// ReasonFleetProgressing: the rollout is opening waves normally.
	ReasonFleetProgressing = "Progressing"
	// ReasonFleetCompleted: every wave reached the target version.
	ReasonFleetCompleted = "Completed"
	// ReasonFleetMembersUnassigned: a selected StageSet matches no wave, so it
	// would never be approved — a configuration error the rollout refuses to
	// silently skip.
	ReasonFleetMembersUnassigned = "MembersUnassigned"
	// ReasonFleetInvalid: the selector or a wave selector is malformed.
	ReasonFleetInvalid = "InvalidSelector"
	// ReasonFleetMembersContested: a selected StageSet is also selected by another
	// FleetRollout. A member must be governed by at most one rollout — two would
	// fight over its approval annotation — so an overlap fails closed.
	ReasonFleetMembersContested = "MembersContested"
	// ReasonFleetHalted: a wave failed its health gate or a settled member
	// regressed; no further waves open until the cause clears.
	ReasonFleetHalted = "Halted"
)

// Wave health-gate verdicts recorded in FleetWaveStatus.Health.
const (
	healthPassing = "Passing"
	healthFailing = "Failing"
	healthUnknown = "Unknown"
)

// fleetRequeueInterval bounds how long a progressing rollout waits before
// re-checking members, in addition to the StageSet watch that wakes it on change.
const fleetRequeueInterval = 30 * time.Second

// FleetRolloutReconciler advances a FleetRollout: it approves its target version
// across the selected StageSets one wave at a time, opening the next wave only
// once the current wave's members have all reached the version and gone Ready.
type FleetRolloutReconciler struct {
	client.Client
	// APIReader is an uncached, cluster-wide reader for the member StageSets — the
	// cached client may be namespace-scoped (--watch-namespaces), but a fleet spans
	// namespaces. Defaults to mgr.GetAPIReader(); tests may leave it nil to fall
	// back to the (fake) client.
	APIReader client.Reader
	// MetricQuerier resolves a wave gate's MetricSource to a scalar. Defaulted in
	// SetupWithManager; tests substitute a fake.
	MetricQuerier metricsource.Querier
	// Recorder emits Events on halt and completion; nil disables them.
	Recorder events.EventRecorder
	// Now returns the current time for soak timing; nil defaults to time.Now.
	Now func() time.Time
}

func (r *FleetRolloutReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=stages.metio.wtf,resources=fleetrollouts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stages.metio.wtf,resources=fleetrollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stages.metio.wtf,resources=stagesets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile advances one FleetRollout and patches its status.
func (r *FleetRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var fr stagesv1.FleetRollout
	if err := r.Get(ctx, req.NamespacedName, &fr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	helper, err := fluxpatch.NewHelper(&fr, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	result, rerr := r.reconcile(ctx, &fr)

	if perr := helper.Patch(ctx, &fr, fluxpatch.WithOwnedConditions{Conditions: []string{ConditionReady}}); perr != nil && rerr == nil {
		rerr = perr
	}
	return result, rerr
}

func (r *FleetRolloutReconciler) reconcile(ctx context.Context, fr *stagesv1.FleetRollout) (ctrl.Result, error) {
	fr.Status.ObservedGeneration = fr.Generation
	target := fr.Spec.TargetVersion

	members, err := r.members(ctx, fr)
	if err != nil {
		r.setFleetReady(fr, metav1.ConditionFalse, ReasonFleetInvalid, err.Error())
		return ctrl.Result{}, nil // a malformed selector is terminal until the spec changes
	}

	// A member governed by another FleetRollout would have both stamp its approval
	// annotation and fight over it, so an overlap fails closed — nothing is stamped
	// until the contention is resolved.
	if contested, cerr := r.contestedMembers(ctx, fr, members); cerr != nil {
		return ctrl.Result{}, cerr
	} else if len(contested) > 0 {
		r.setFleetReady(fr, metav1.ConditionFalse, ReasonFleetMembersContested,
			fmt.Sprintf("%d selected StageSet(s) are governed by another FleetRollout: %s", len(contested), strings.Join(contested, ", ")))
		return ctrl.Result{RequeueAfter: fleetRequeueInterval}, nil
	}

	// Partition members into waves and detect any that match no wave.
	waveSelectors := make([]labels.Selector, len(fr.Spec.Waves))
	for i := range fr.Spec.Waves {
		sel, serr := metav1.LabelSelectorAsSelector(&fr.Spec.Waves[i].Selector)
		if serr != nil {
			r.setFleetReady(fr, metav1.ConditionFalse, ReasonFleetInvalid,
				fmt.Sprintf("wave %q selector: %v", fr.Spec.Waves[i].Name, serr))
			return ctrl.Result{}, nil
		}
		waveSelectors[i] = sel
	}
	waveMembers := make([][]stagesv1.StageSet, len(fr.Spec.Waves))
	var unassigned []string
	for i := range members {
		m := &members[i]
		matched := false
		for w := range waveSelectors {
			if waveSelectors[w].Matches(labels.Set(m.Labels)) {
				waveMembers[w] = append(waveMembers[w], *m)
				matched = true
			}
		}
		if !matched {
			unassigned = append(unassigned, m.Namespace+"/"+m.Name)
		}
	}
	if len(unassigned) > 0 {
		sort.Strings(unassigned)
		r.setFleetReady(fr, metav1.ConditionFalse, ReasonFleetMembersUnassigned,
			fmt.Sprintf("%d selected StageSet(s) match no wave: %s", len(unassigned), strings.Join(unassigned, ", ")))
		return ctrl.Result{RequeueAfter: fleetRequeueInterval}, nil
	}

	now := r.now()
	prior := make(map[string]stagesv1.FleetWaveStatus, len(fr.Status.Waves))
	for _, ws := range fr.Status.Waves {
		prior[ws.Name] = ws
	}

	// Pass 1: compute each wave's settle status, carrying its soak deadline (set
	// the moment it first settles) and prior health forward.
	fr.Status.Waves = make([]stagesv1.FleetWaveStatus, len(fr.Spec.Waves))
	for w := range fr.Spec.Waves {
		ws := waveStatus(fr.Spec.Waves[w].Name, waveMembers[w], target)
		prev := prior[ws.Name]
		ws.Health = prev.Health
		if ws.Settled {
			if prev.SoakUntil != nil {
				ws.SoakUntil = prev.SoakUntil
			} else {
				soak := time.Duration(0)
				if s := fr.Spec.Waves[w].Soak; s != nil {
					soak = s.Duration
				}
				until := metav1.NewTime(now.Add(soak))
				ws.SoakUntil = &until
			}
		}
		fr.Status.Waves[w] = ws
	}

	// Pass 2: walk waves in order. A wave that had begun soaking but is no longer
	// settled has a member that regressed → halt the fleet. A wave passes when it
	// is settled, its soak has elapsed, and its health gate (if any) is satisfied;
	// a gate the scalar violates halts the fleet. Open every wave through the first
	// not-yet-passed one; the rest stay held.
	firstOpen := -1
	for w := range fr.Spec.Waves {
		ws := &fr.Status.Waves[w]
		if prior[ws.Name].SoakUntil != nil && !ws.Settled {
			return r.halt(ctx, fr, ws.Name, waveMembers[w],
				fmt.Sprintf("wave %q regressed: a member is no longer at version %s and Ready", ws.Name, target))
		}
		passed := false
		if ws.Settled && !now.Before(ws.SoakUntil.Time) {
			gate := fr.Spec.Waves[w].Gate
			if gate == nil {
				passed = true
			} else {
				verdict, value, gerr := r.evalGate(ctx, gate)
				ws.Health = verdict
				switch {
				case gerr != nil, verdict == healthUnknown:
					// A metric outage neither advances nor halts — hold this wave.
				case verdict == healthPassing:
					passed = true
				default: // healthFailing
					return r.halt(ctx, fr, ws.Name, waveMembers[w],
						fmt.Sprintf("wave %q failed its health gate: metric %.4g is outside the threshold", ws.Name, value))
				}
			}
		}
		if firstOpen == -1 && !passed {
			firstOpen = w
		}
	}

	// Open every wave through the first not-yet-passed one by stamping the approval;
	// earlier waves are already at the target, so their stamps are no-ops.
	openThrough := firstOpen
	if firstOpen == -1 {
		openThrough = len(fr.Spec.Waves) - 1
	}
	for w := 0; w <= openThrough; w++ {
		for i := range waveMembers[w] {
			if err := r.approve(ctx, &waveMembers[w][i], target); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if firstOpen == -1 {
		fr.Status.Phase = stagesv1.FleetCompleted
		fr.Status.CurrentWave = ""
		r.setFleetReady(fr, metav1.ConditionTrue, ReasonFleetCompleted,
			fmt.Sprintf("all %d wave(s) reached version %s", len(fr.Spec.Waves), target))
		return ctrl.Result{}, nil
	}

	fr.Status.Phase = stagesv1.FleetInProgress
	fr.Status.CurrentWave = fr.Spec.Waves[firstOpen].Name
	r.setFleetReady(fr, metav1.ConditionTrue, ReasonFleetProgressing,
		fmt.Sprintf("rolling out version %s; wave %q open", target, fr.Status.CurrentWave))
	return ctrl.Result{RequeueAfter: r.requeueAfter(fr, now)}, nil
}

// requeueAfter wakes a soaking wave exactly when its soak elapses (so the gate is
// evaluated promptly), and otherwise falls back to the periodic interval that
// backstops the StageSet watch.
func (r *FleetRolloutReconciler) requeueAfter(fr *stagesv1.FleetRollout, now time.Time) time.Duration {
	for w := range fr.Status.Waves {
		ws := &fr.Status.Waves[w]
		if ws.Settled && ws.SoakUntil != nil && ws.SoakUntil.Time.After(now) {
			if d := ws.SoakUntil.Time.Sub(now); d < fleetRequeueInterval {
				return d
			}
		}
	}
	return fleetRequeueInterval
}

// evalGate queries a wave gate's metric and reports the verdict and the scalar.
// A query or threshold-parse error resolves to Unknown (hold, neither pass nor
// halt) — a metric outage must not auto-advance or auto-halt the fleet.
func (r *FleetRolloutReconciler) evalGate(ctx context.Context, gate *stagesv1.FleetWaveGate) (verdict string, value float64, err error) {
	if r.MetricQuerier == nil {
		return healthUnknown, 0, fmt.Errorf("no metric querier configured")
	}
	// The gate is a fleet-level query run under the controller's own identity.
	value, err = r.MetricQuerier.Query(ctx, "", "", gate.Source)
	if err != nil {
		return healthUnknown, 0, err
	}
	ok, terr := metricsource.ThresholdSatisfied(gate.Threshold, value)
	if terr != nil {
		return healthUnknown, value, terr
	}
	if ok {
		return healthPassing, value, nil
	}
	return healthFailing, value, nil
}

// halt stops the rollout: no further waves open, phase becomes Halted, and a
// Warning event records the cause. When onRegression is Rollback it first directs
// the halted wave's members back to previousVersion (they revert via their own
// down migrations, or refuse if not reversible). The halt is re-derived each
// reconcile, so it clears on its own if the cause does.
func (r *FleetRolloutReconciler) halt(ctx context.Context, fr *stagesv1.FleetRollout, wave string, waveMembers []stagesv1.StageSet, msg string) (ctrl.Result, error) {
	if fr.Spec.OnRegression == "Rollback" && fr.Spec.PreviousVersion != "" {
		for i := range waveMembers {
			if err := r.rollbackMember(ctx, &waveMembers[i], fr.Spec.PreviousVersion); err != nil {
				return ctrl.Result{}, err
			}
		}
		msg += fmt.Sprintf("; rolling wave %q back to %s", wave, fr.Spec.PreviousVersion)
	}
	fr.Status.Phase = stagesv1.FleetHalted
	fr.Status.CurrentWave = wave
	r.setFleetReady(fr, metav1.ConditionFalse, ReasonFleetHalted, msg)
	if r.Recorder != nil {
		r.Recorder.Eventf(fr, nil, corev1.EventTypeWarning, ReasonFleetHalted, ReasonFleetHalted, "%s", msg)
	}
	return ctrl.Result{RequeueAfter: fleetRequeueInterval}, nil
}

// rollbackMember stamps the rollback-to annotation, directing a member to revert
// to version. Idempotent.
func (r *FleetRolloutReconciler) rollbackMember(ctx context.Context, ss *stagesv1.StageSet, version string) error {
	if ss.Annotations[rollbackToAnnotation] == version {
		return nil
	}
	patch := client.MergeFrom(ss.DeepCopy())
	if ss.Annotations == nil {
		ss.Annotations = map[string]string{}
	}
	ss.Annotations[rollbackToAnnotation] = version
	return r.Patch(ctx, ss, patch)
}

// members resolves the StageSets this rollout targets: those matching spec.selector,
// bounded by spec.namespaceSelector (nil = every namespace). Reads go through the
// uncached, cluster-wide reader.
func (r *FleetRolloutReconciler) members(ctx context.Context, fr *stagesv1.FleetRollout) ([]stagesv1.StageSet, error) {
	sel, err := metav1.LabelSelectorAsSelector(&fr.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("selector: %w", err)
	}
	var list stagesv1.StageSetList
	if err := r.reader().List(ctx, &list, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	if fr.Spec.NamespaceSelector == nil {
		return list.Items, nil
	}
	nsSel, err := metav1.LabelSelectorAsSelector(fr.Spec.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("namespaceSelector: %w", err)
	}
	var nsList corev1.NamespaceList
	if err := r.reader().List(ctx, &nsList, client.MatchingLabelsSelector{Selector: nsSel}); err != nil {
		return nil, err
	}
	inScope := make(map[string]bool, len(nsList.Items))
	for i := range nsList.Items {
		inScope[nsList.Items[i].Name] = true
	}
	out := list.Items[:0]
	for i := range list.Items {
		if inScope[list.Items[i].Namespace] {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// contestedMembers returns the members of this rollout that another FleetRollout
// also selects, formatted "namespace/name (also governed by FleetRollout <name>)".
// It reuses members() against each other rollout so the same selector +
// namespaceSelector semantics decide overlap; a broken other rollout is skipped
// rather than blocking this one.
func (r *FleetRolloutReconciler) contestedMembers(ctx context.Context, fr *stagesv1.FleetRollout, members []stagesv1.StageSet) ([]string, error) {
	var others stagesv1.FleetRolloutList
	if err := r.List(ctx, &others); err != nil {
		return nil, err
	}
	claimedBy := map[string]string{} // "ns/name" -> other rollout name
	for i := range others.Items {
		o := &others.Items[i]
		if o.Name == fr.Name {
			continue
		}
		oMembers, err := r.members(ctx, o)
		if err != nil {
			continue // a malformed other rollout must not block this one
		}
		for j := range oMembers {
			key := oMembers[j].Namespace + "/" + oMembers[j].Name
			if _, seen := claimedBy[key]; !seen {
				claimedBy[key] = o.Name
			}
		}
	}
	var conflicts []string
	for i := range members {
		key := members[i].Namespace + "/" + members[i].Name
		if other, ok := claimedBy[key]; ok {
			conflicts = append(conflicts, fmt.Sprintf("%s (also governed by FleetRollout %s)", key, other))
		}
	}
	sort.Strings(conflicts)
	return conflicts, nil
}

// approve stamps the target version onto a member's approved-version annotation,
// releasing its held transition. Idempotent: a member already approved is skipped.
func (r *FleetRolloutReconciler) approve(ctx context.Context, ss *stagesv1.StageSet, target string) error {
	if ss.Annotations[approvedVersionAnnotation] == target {
		return nil
	}
	patch := client.MergeFrom(ss.DeepCopy())
	if ss.Annotations == nil {
		ss.Annotations = map[string]string{}
	}
	ss.Annotations[approvedVersionAnnotation] = target
	return r.Patch(ctx, ss, patch)
}

func (r *FleetRolloutReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// waveStatus counts a wave's members that have reached the target version and are
// Ready, and marks the wave settled when every member has. An empty wave is
// settled — there is nothing to wait for.
func waveStatus(name string, members []stagesv1.StageSet, target string) stagesv1.FleetWaveStatus {
	ws := stagesv1.FleetWaveStatus{Name: name, Total: int32(len(members))}
	for i := range members {
		atTarget, ready := memberProgress(&members[i], target)
		if atTarget {
			ws.AtTarget++
		}
		if ready {
			ws.Ready++
		}
	}
	ws.Settled = ws.AtTarget == ws.Total && ws.Ready == ws.Total
	return ws
}

// memberProgress reports whether a StageSet has reached the target version and is
// healthy at its current spec — Ready, fully reconciled, with no new revision held
// back (the same "usable" predicate dependenciesReady applies to a dependency).
func memberProgress(ss *stagesv1.StageSet, target string) (atTarget, ready bool) {
	atTarget = ss.Status.Version == target
	ready = isReady(ss) &&
		ss.Status.ObservedGeneration == ss.Generation &&
		(ss.Status.PendingUpdate == nil || len(ss.Status.PendingUpdate.Revisions) == 0)
	return atTarget, ready
}

func (r *FleetRolloutReconciler) setFleetReady(fr *stagesv1.FleetRollout, status metav1.ConditionStatus, reason, message string) {
	fluxconditions.Set(fr, &metav1.Condition{
		Type:    ConditionReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

// mapStageSetToFleets enqueues every FleetRollout whose top-level selector matches
// a changed StageSet, so a member reaching the target version (or regressing) wakes
// its rollout. The namespaceSelector is applied in Reconcile, not here — a few
// extra wake-ups are cheaper than listing namespaces per event.
func (r *FleetRolloutReconciler) mapStageSetToFleets(ctx context.Context, obj client.Object) []reconcile.Request {
	ss, ok := obj.(*stagesv1.StageSet)
	if !ok {
		return nil
	}
	var fleets stagesv1.FleetRolloutList
	if err := r.List(ctx, &fleets); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range fleets.Items {
		sel, err := metav1.LabelSelectorAsSelector(&fleets.Items[i].Spec.Selector)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(ss.Labels)) {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: fleets.Items[i].Name}})
		}
	}
	return reqs
}

// SetupWithManager registers the FleetRollout reconciler, watching StageSets so a
// member reaching the target or going not-Ready re-drives the owning rollout.
func (r *FleetRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("fleetrollout-controller")
	}
	if r.MetricQuerier == nil {
		// A fleet gate is a controller-identity query; the client-backed secret
		// reader resolves bearer-token auth when the gate names a secret, and the
		// production IP denylist (nil validator) still guards against SSRF.
		r.MetricQuerier = metricsource.New(r.readSecret, nil, nil)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&stagesv1.FleetRollout{}).
		Watches(&stagesv1.StageSet{}, handler.EnqueueRequestsFromMapFunc(r.mapStageSetToFleets)).
		Complete(r)
}

// readSecret is the SecretReader for the default metric querier: it reads a
// Secret's data by name in the namespace the query supplies.
func (r *FleetRolloutReconciler) readSecret(ctx context.Context, namespace, _ string, name string) (map[string][]byte, error) {
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &s); err != nil {
		return nil, err
	}
	return s.Data, nil
}
