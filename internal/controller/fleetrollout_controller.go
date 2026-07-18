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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
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

	// Compute per-wave settle status and find the first wave not yet settled.
	fr.Status.Waves = make([]stagesv1.FleetWaveStatus, len(fr.Spec.Waves))
	firstUnsettled := -1
	for w := range fr.Spec.Waves {
		ws := waveStatus(fr.Spec.Waves[w].Name, waveMembers[w], target)
		fr.Status.Waves[w] = ws
		if firstUnsettled == -1 && !ws.Settled {
			firstUnsettled = w
		}
	}

	// Open every wave through the first unsettled one by stamping the approval;
	// prior waves are already at the target, so their stamps are no-ops. Future
	// waves stay held until their turn.
	openThrough := firstUnsettled
	if firstUnsettled == -1 {
		openThrough = len(fr.Spec.Waves) - 1 // all settled: everyone is approved
	}
	for w := 0; w <= openThrough; w++ {
		for i := range waveMembers[w] {
			if err := r.approve(ctx, &waveMembers[w][i], target); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if firstUnsettled == -1 {
		fr.Status.Phase = stagesv1.FleetCompleted
		fr.Status.CurrentWave = ""
		r.setFleetReady(fr, metav1.ConditionTrue, ReasonFleetCompleted,
			fmt.Sprintf("all %d wave(s) reached version %s", len(fr.Spec.Waves), target))
		return ctrl.Result{}, nil
	}

	fr.Status.Phase = stagesv1.FleetInProgress
	fr.Status.CurrentWave = fr.Spec.Waves[firstUnsettled].Name
	r.setFleetReady(fr, metav1.ConditionTrue, ReasonFleetProgressing,
		fmt.Sprintf("rolling out version %s; wave %q open", target, fr.Status.CurrentWave))
	return ctrl.Result{RequeueAfter: fleetRequeueInterval}, nil
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&stagesv1.FleetRollout{}).
		Watches(&stagesv1.StageSet{}, handler.EnqueueRequestsFromMapFunc(r.mapStageSetToFleets)).
		Complete(r)
}
