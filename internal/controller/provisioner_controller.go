/*
Copyright 2026 mohammedamrath.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	xv1 "github.com/amrathhugo/xenonite/api/v1"
)

const (
	// reevaluateInterval re-runs a pool's reconcile so scale-down can act on
	// nodes that became idle without generating an event we watch.
	reevaluateInterval = 30 * time.Second

	// emptyGracePeriod is how long a node must stay empty before its claim is
	// deleted, to avoid churning nodes between brief scheduling gaps.
	emptyGracePeriod = 5 * time.Minute

	// annotationEmptySince records when a claim's node was first observed empty.
	annotationEmptySince = "xenonite.io/empty-since"
)

// ProvisionerReconciler turns scheduling pressure into XenonNodeClaims. It
// reconciles one XenonNodePool at a time: scaling up to satisfy unschedulable
// pods that target the pool, and scaling down claims whose nodes sit idle.
type ProvisionerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.xenonite.io,resources=xenonnodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.xenonite.io,resources=xenonnodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.xenonite.io,resources=xenonnodeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *ProvisionerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pool := &xv1.XenonNodePool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	pendingPods, err := r.pendingPodsForPool(ctx, pool.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	claims, err := r.claimsForPool(ctx, pool.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.scaleUp(ctx, pool, pendingPods, claims); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.scaleDown(ctx, pool, pendingPods, claims); err != nil {
		return ctrl.Result{}, err
	}

	r.updateStatus(ctx, pool, len(claims))
	log.V(1).Info("reconciled pool", "pending", len(pendingPods), "claims", len(claims))
	return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
}

// scaleUp creates one claim per pending pod that isn't already covered by an
// in-flight (not-yet-ready) claim. This intentionally favors availability over
// tight bin-packing; see the package notes for the trade-off.
func (r *ProvisionerReconciler) scaleUp(
	ctx context.Context, pool *xv1.XenonNodePool, pending []corev1.Pod, claims []xv1.XenonNodeClaim,
) error {
	log := logf.FromContext(ctx)
	if len(pending) == 0 {
		return nil
	}

	inFlight := 0
	for i := range claims {
		if !isClaimReady(&claims[i]) {
			inFlight++
		}
	}
	needed := len(pending) - inFlight
	if max := poolNodeLimit(pool); max > 0 && len(claims)+needed > max {
		needed = max - len(claims)
	}
	for i := 0; i < needed; i++ {
		if err := r.createClaim(ctx, pool); err != nil {
			return err
		}
		log.Info("created claim for pending pods", "pool", pool.Name)
	}
	return nil
}

// scaleDown deletes claims whose node has been empty past the grace period and
// that aren't needed to absorb currently-pending pods.
func (r *ProvisionerReconciler) scaleDown(
	ctx context.Context, pool *xv1.XenonNodePool, pending []corev1.Pod, claims []xv1.XenonNodeClaim,
) error {
	log := logf.FromContext(ctx)
	// If pods are still pending for this pool, keep every node — they may land here.
	if len(pending) > 0 {
		for i := range claims {
			r.clearEmptySince(ctx, &claims[i])
		}
		return nil
	}

	for i := range claims {
		claim := &claims[i]
		if !claim.DeletionTimestamp.IsZero() {
			continue
		}
		empty, err := r.isClaimNodeEmpty(ctx, claim)
		if err != nil {
			return err
		}
		if !empty {
			r.clearEmptySince(ctx, claim)
			continue
		}
		expired, err := r.markEmptyAndCheckGrace(ctx, claim)
		if err != nil {
			return err
		}
		if expired {
			log.Info("deleting idle claim", "claim", claim.Name)
			if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *ProvisionerReconciler) createClaim(ctx context.Context, pool *xv1.XenonNodePool) error {
	claim := &xv1.XenonNodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pool.Name + "-",
			Labels:       map[string]string{xv1.LabelNodePool: pool.Name},
		},
		Spec: xv1.XenonNodeClaimSpec{
			NodePoolRef:  corev1.LocalObjectReference{Name: pool.Name},
			Requirements: pool.Spec.Template.Spec.Requirements,
			Resources:    pool.Spec.Template.Spec.Resources,
		},
	}
	// Owned by the pool so deleting the pool garbage-collects its claims.
	if err := controllerutil.SetControllerReference(pool, claim, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, claim)
}

// pendingPodsForPool returns unschedulable pods that select this pool.
func (r *ProvisionerReconciler) pendingPodsForPool(ctx context.Context, poolName string) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for i := range pods.Items {
		p := pods.Items[i]
		if isPodUnschedulable(&p) && podRequestsPool(&p, poolName) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *ProvisionerReconciler) claimsForPool(ctx context.Context, poolName string) ([]xv1.XenonNodeClaim, error) {
	var claims xv1.XenonNodeClaimList
	if err := r.List(ctx, &claims, client.MatchingLabels{xv1.LabelNodePool: poolName}); err != nil {
		return nil, err
	}
	return claims.Items, nil
}

// isClaimNodeEmpty reports whether the claim's node hosts no reschedulable
// workload pods (DaemonSet, mirror/static, and terminating pods don't count).
// A claim with no node yet is not considered empty (it's still provisioning).
func (r *ProvisionerReconciler) isClaimNodeEmpty(ctx context.Context, claim *xv1.XenonNodeClaim) (bool, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return false, err
	}
	for i := range pods.Items {
		if isReschedulablePod(&pods.Items[i]) {
			return false, nil
		}
	}
	return true, nil
}

// markEmptyAndCheckGrace stamps the first-empty time on the claim and reports
// whether the grace period has elapsed.
func (r *ProvisionerReconciler) markEmptyAndCheckGrace(ctx context.Context, claim *xv1.XenonNodeClaim) (bool, error) {
	now := time.Now()
	if claim.Annotations[annotationEmptySince] == "" {
		patch := client.MergeFrom(claim.DeepCopy())
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[annotationEmptySince] = now.UTC().Format(time.RFC3339)
		return false, r.Patch(ctx, claim, patch)
	}
	since, err := time.Parse(time.RFC3339, claim.Annotations[annotationEmptySince])
	if err != nil {
		// Corrupt value: reset it and wait another grace period.
		patch := client.MergeFrom(claim.DeepCopy())
		claim.Annotations[annotationEmptySince] = now.UTC().Format(time.RFC3339)
		return false, r.Patch(ctx, claim, patch)
	}
	return now.Sub(since) >= emptyGracePeriod, nil
}

func (r *ProvisionerReconciler) clearEmptySince(ctx context.Context, claim *xv1.XenonNodeClaim) {
	if claim.Annotations[annotationEmptySince] == "" {
		return
	}
	patch := client.MergeFrom(claim.DeepCopy())
	delete(claim.Annotations, annotationEmptySince)
	_ = r.Patch(ctx, claim, patch)
}

func (r *ProvisionerReconciler) updateStatus(ctx context.Context, pool *xv1.XenonNodePool, claimCount int) {
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:    "Active",
		Status:  metav1.ConditionTrue,
		Reason:  "Reconciled",
		Message: "pool reconciled",
	})
	_ = r.Status().Update(ctx, pool)
}

// --- pure helpers -----------------------------------------------------------

// podRequestsPool reports whether the pod targets poolName via nodeSelector or a
// required node-affinity term on the pool label.
func podRequestsPool(pod *corev1.Pod, poolName string) bool {
	if pod.Spec.NodeSelector[xv1.LabelNodePool] == poolName {
		return true
	}
	aff := pod.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != xv1.LabelNodePool || expr.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			for _, v := range expr.Values {
				if v == poolName {
					return true
				}
			}
		}
	}
	return false
}

func isPodUnschedulable(pod *corev1.Pod) bool {
	if pod.Spec.NodeName != "" || pod.Status.Phase != corev1.PodPending {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

// isReschedulablePod is true for pods that would move elsewhere if the node went
// away — i.e. real workload. DaemonSet, static/mirror, and terminating pods are
// excluded so they never keep an otherwise-idle node alive.
func isReschedulablePod(pod *corev1.Pod) bool {
	if !pod.DeletionTimestamp.IsZero() {
		return false
	}
	if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
		return false
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return false
		}
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	return true
}

func isClaimReady(claim *xv1.XenonNodeClaim) bool {
	return apimeta.IsStatusConditionTrue(claim.Status.Conditions, "Ready")
}

// poolNodeLimit caps the number of claims per pool via a "nodes" entry in
// spec.limits. 0 (no entry) means unbounded.
func poolNodeLimit(pool *xv1.XenonNodePool) int {
	if q, ok := pool.Spec.Limits["nodes"]; ok {
		return int(q.Value())
	}
	return 0
}

// --- watch wiring -----------------------------------------------------------

func (r *ProvisionerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index pods by node name so isClaimNodeEmpty can list a node's pods cheaply.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName",
		func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&xv1.XenonNodePool{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.poolForPod)).
		Watches(&xv1.XenonNodeClaim{}, handler.EnqueueRequestsFromMapFunc(r.poolForClaim)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.poolForNode)).
		Named("provisioner").
		Complete(r)
}

// poolForPod enqueues the pool a pending pod targets. We only have the requested
// pool name from the label; if the pod doesn't request one, nothing is enqueued.
func (r *ProvisionerReconciler) poolForPod(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	name := requestedPoolName(pod)
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

func (r *ProvisionerReconciler) poolForClaim(_ context.Context, obj client.Object) []reconcile.Request {
	claim, ok := obj.(*xv1.XenonNodeClaim)
	if !ok || claim.Spec.NodePoolRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: claim.Spec.NodePoolRef.Name}}}
}

func (r *ProvisionerReconciler) poolForNode(_ context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	name := node.Labels[xv1.LabelNodePool]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

// requestedPoolName extracts the pool a pod targets from its nodeSelector or
// required node affinity.
func requestedPoolName(pod *corev1.Pod) string {
	if v := pod.Spec.NodeSelector[xv1.LabelNodePool]; v != "" {
		return v
	}
	aff := pod.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	for _, term := range aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == xv1.LabelNodePool && expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
				return expr.Values[0]
			}
		}
	}
	return ""
}
