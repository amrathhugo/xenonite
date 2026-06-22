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
	"bytes"
	"context"
	"fmt"
	"text/template"
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/luthermonson/go-proxmox"

	xv1 "github.com/amrathhugo/xenonite/api/v1"
	"github.com/amrathhugo/xenonite/internal/provider"
	proxmoxprovider "github.com/amrathhugo/xenonite/internal/provider/proxmox"
)

// nodeRegistrationPoll is how often we re-check whether the freshly created VM
// has registered as a Kubernetes Node. Node join is expected to take time, so
// this is steady polling
const nodeRegistrationPoll = 15 * time.Second

// XenonNodeClaimReconciler owns the lifecycle of a single XenonNodeClaim: it
// provisions exactly one backing machine and tears it down via a finalizer.
type XenonNodeClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewProvider builds a Provider for a resolved ProxmoxProvider. Injectable so
	// tests can supply a fake. Defaults to the proxmox implementation.
	NewProvider func(spec xv1.ProxmoxProviderSpec, creds *proxmox.Credentials) provider.Provider

	// SystemNamespace is the operator's namespace, where cluster-global Secrets
	// (e.g. the bootstrap join token) live. Set from POD_NAMESPACE at startup.
	SystemNamespace string
}

// +kubebuilder:rbac:groups=core.xenonite.io,resources=xenonnodeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.xenonite.io,resources=xenonnodeclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.xenonite.io,resources=xenonnodeclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.xenonite.io,resources=xenonnodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.xenonite.io,resources=proxmoxproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *XenonNodeClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	claim := &xv1.XenonNodeClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deleting: run finalizer teardown.
	if !claim.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, claim)
	}

	// Ensure the finalizer is present before we create anything, so a crash
	// between create and the next reconcile can never orphan a VM.
	if controllerutil.AddFinalizer(claim, xv1.FinalizerNodeClaim) {
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	prov, provSpec, err := r.resolveProvider(ctx, claim)
	if err != nil {
		r.setCondition(claim, "Ready", metav1.ConditionFalse, "ProviderResolveFailed", err.Error())
		_ = r.Status().Update(ctx, claim)
		// Configuration errors won't fix themselves on a tight loop, but retrying
		// with backoff covers the "secret not created yet" race.
		return ctrl.Result{}, err
	}

	// Provision once: a non-empty providerID means the VM already exists.
	if claim.Status.ProviderID == "" {
		return r.reconcileCreate(ctx, claim, prov, provSpec)
	}

	// Provisioned: wait for the node to join and mark Ready.
	return r.reconcileReadiness(ctx, claim)
}

// reconcileCreate renders cloud-init and provisions the VM. On any failure it
// persists whatever providerID the provider returned (so a partially created VM
// is still tracked for cleanup) and returns the error for exponential backoff.
func (r *XenonNodeClaimReconciler) reconcileCreate(
	ctx context.Context, claim *xv1.XenonNodeClaim, prov provider.Provider, spec *xv1.ProxmoxProviderSpec,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	userData, err := r.renderCloudInit(ctx, claim, spec)
	if err != nil {
		r.setCondition(claim, "Ready", metav1.ConditionFalse, "CloudInitRenderFailed", err.Error())
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	cores, memMB, diskGB := requestedSize(claim)
	providerID, createErr := prov.Create(ctx, provider.CreateRequest{
		Name:              claim.Name,
		Cores:             cores,
		MemoryMB:          memMB,
		DiskGB:            diskGB,
		CloudInitUserData: userData,
	})

	// Record the providerID even on partial failure so teardown can find the VM.
	if providerID != "" && claim.Status.ProviderID == "" {
		claim.Status.ProviderID = providerID
	}
	if createErr != nil {
		r.setCondition(claim, "Launched", metav1.ConditionFalse, "CreateFailed", createErr.Error())
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, createErr // exponential backoff via the workqueue
	}

	r.setCondition(claim, "Launched", metav1.ConditionTrue, "Created", "backing machine created")
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("provisioned backing machine", "providerID", providerID)
	return ctrl.Result{RequeueAfter: nodeRegistrationPoll}, nil
}

// reconcileReadiness looks for the Node the VM registered as, labels it with its
// pool/claim, and flips Ready once the kubelet reports Ready.
func (r *XenonNodeClaimReconciler) reconcileReadiness(ctx context.Context, claim *xv1.XenonNodeClaim) (ctrl.Result, error) {
	node := &corev1.Node{}
	// The VM's hostname is the claim name, so the Node registers under that name.
	err := r.Get(ctx, types.NamespacedName{Name: claim.Name}, node)
	if apierrors.IsNotFound(err) {
		r.setCondition(claim, "Ready", metav1.ConditionFalse, "NodeNotRegistered", "waiting for node to join")
		if err := r.Status().Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: nodeRegistrationPoll}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Stamp the pool/claim labels so the scheduler binds pending pods here.
	if err := r.labelNode(ctx, node, claim); err != nil {
		return ctrl.Result{}, err
	}

	if isNodeReady(node) {
		r.setCondition(claim, "Ready", metav1.ConditionTrue, "NodeReady", "node registered and ready")
	} else {
		r.setCondition(claim, "Ready", metav1.ConditionFalse, "NodeNotReady", "node registered, kubelet not ready")
	}
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	if !isNodeReady(node) {
		return ctrl.Result{RequeueAfter: nodeRegistrationPoll}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileDelete deletes the backing VM and confirms it is gone before dropping
// the finalizer. Both the delete call and the "still exists" verification return
// errors so the workqueue retries with exponential backoff.
func (r *XenonNodeClaimReconciler) reconcileDelete(ctx context.Context, claim *xv1.XenonNodeClaim) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(claim, xv1.FinalizerNodeClaim) {
		return ctrl.Result{}, nil
	}

	// Nothing was ever provisioned — just release.
	if claim.Status.ProviderID == "" {
		return r.removeFinalizer(ctx, claim)
	}

	prov, _, err := r.resolveProvider(ctx, claim)
	if err != nil {
		// Provider config is gone (e.g. ProxmoxProvider deleted first). We cannot
		// safely delete the VM; surface and retry rather than orphan it silently.
		return ctrl.Result{}, fmt.Errorf("cannot resolve provider for cleanup: %w", err)
	}

	if err := prov.Delete(ctx, claim.Status.ProviderID); err != nil {
		return ctrl.Result{}, err
	}

	// Verify the VM is actually gone before releasing the finalizer.
	inst, err := prov.Get(ctx, claim.Status.ProviderID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if inst != nil {
		return ctrl.Result{}, fmt.Errorf("machine %s still exists after delete; retrying", claim.Status.ProviderID)
	}

	log.Info("backing machine deleted", "providerID", claim.Status.ProviderID)
	return r.removeFinalizer(ctx, claim)
}

func (r *XenonNodeClaimReconciler) removeFinalizer(ctx context.Context, claim *xv1.XenonNodeClaim) (ctrl.Result, error) {
	if controllerutil.RemoveFinalizer(claim, xv1.FinalizerNodeClaim) {
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// resolveProvider walks claim -> XenonNodePool -> ProxmoxProvider and returns a
// ready-to-use Provider plus the provider spec (needed for cloud-init).
// Credentials are read inline from the ProxmoxProvider spec.
func (r *XenonNodeClaimReconciler) resolveProvider(
	ctx context.Context, claim *xv1.XenonNodeClaim,
) (provider.Provider, *xv1.ProxmoxProviderSpec, error) {
	pool := &xv1.XenonNodePool{}
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.NodePoolRef.Name}, pool); err != nil {
		return nil, nil, fmt.Errorf("get nodepool %q: %w", claim.Spec.NodePoolRef.Name, err)
	}

	ref := pool.Spec.ProviderRef
	if ref.Kind != "ProxmoxProvider" {
		return nil, nil, fmt.Errorf("unsupported provider kind %q", ref.Kind)
	}
	pp := &xv1.ProxmoxProvider{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, pp); err != nil {
		return nil, nil, fmt.Errorf("get proxmoxprovider %q: %w", ref.Name, err)
	}

	creds := proxmoxCredentials(pp.Spec.Credentials)

	factory := r.NewProvider
	if factory == nil {
		factory = func(s xv1.ProxmoxProviderSpec, c *proxmox.Credentials) provider.Provider {
			return proxmoxprovider.New(s, c)
		}
	}
	return factory(pp.Spec, creds), &pp.Spec, nil
}

// proxmoxCredentials builds the go-proxmox credentials from the inline spec
// fields. Credentials live directly on the ProxmoxProvider CR, so no Secret
// lookup is required.
func proxmoxCredentials(c xv1.ProxmoxCredentials) *proxmox.Credentials {
	return &proxmox.Credentials{
		Username: c.Username,
		Password: c.Password,
		Realm:    c.Realm,
	}
}

// renderCloudInit fills the provider's user-data template with this node's
// values and the join token fetched from the bootstrap secret.
func (r *XenonNodeClaimReconciler) renderCloudInit(
	ctx context.Context, claim *xv1.XenonNodeClaim, spec *xv1.ProxmoxProviderSpec,
) (string, error) {
	token, err := r.readSecretKey(ctx, spec.Bootstrap.TokenSecretRef)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New("cloudinit").Parse(spec.CloudInit.UserData)
	if err != nil {
		return "", fmt.Errorf("parse cloud-init template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{
		"NodeName": claim.Name,
		"Server":   spec.Bootstrap.Server,
		"Token":    token,
	}); err != nil {
		return "", fmt.Errorf("render cloud-init template: %w", err)
	}
	return buf.String(), nil
}

func (r *XenonNodeClaimReconciler) readSecretKey(ctx context.Context, ref xv1.SecretKeyRef) (string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: ref.Name, Namespace: r.SystemNamespace}
	if err := r.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("get secret %s: %w", key, err)
	}
	val, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %s has no key %q", key, ref.Key)
	}
	return string(val), nil
}

func (r *XenonNodeClaimReconciler) labelNode(ctx context.Context, node *corev1.Node, claim *xv1.XenonNodeClaim) error {
	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	changed := false
	if node.Labels[xv1.LabelNodePool] != claim.Spec.NodePoolRef.Name {
		node.Labels[xv1.LabelNodePool] = claim.Spec.NodePoolRef.Name
		changed = true
	}
	if node.Labels[xv1.LabelNodeClaim] != claim.Name {
		node.Labels[xv1.LabelNodeClaim] = claim.Name
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, node, patch)
}

func (r *XenonNodeClaimReconciler) setCondition(claim *xv1.XenonNodeClaim, condType string, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:    condType,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
}

func requestedSize(claim *xv1.XenonNodeClaim) (cores, memMB, diskGB int) {
	if cpu, ok := claim.Spec.Resources[corev1.ResourceCPU]; ok {
		cores = int(cpu.Value())
	}
	if mem, ok := claim.Spec.Resources[corev1.ResourceMemory]; ok {
		memMB = int(mem.Value() / (1024 * 1024))
	}
	if disk, ok := claim.Spec.Resources[corev1.ResourceStorage]; ok {
		diskGB = int(disk.Value() / (1024 * 1024 * 1024))
	}
	return cores, memMB, diskGB
}

func isNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *XenonNodeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&xv1.XenonNodeClaim{}).
		Named("xenonnodeclaim").
		Complete(r)
}
