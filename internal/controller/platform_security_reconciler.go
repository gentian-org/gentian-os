/*
Copyright 2026 Gentian Organization.

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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

const (
	conditionPlatformSecurityReady = "PlatformSecurityReady"
	platformSecurityRequeue        = 5 * time.Minute
)

// +kubebuilder:rbac:groups=gentianos.io,resources=platformsecuritypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=platformsecuritypolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=platformsecuritypolicies/finalizers,verbs=update

// PlatformSecurityPolicyReconciler syncs cluster MAC waiver allowlist to a ConfigMap.
type PlatformSecurityPolicyReconciler struct {
	client.Client
	OperatorNamespace string
}

func (r *PlatformSecurityPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if req.Name != gentianov1alpha1.PlatformSecurityPolicyName {
		return ctrl.Result{}, nil
	}

	psp := &gentianov1alpha1.PlatformSecurityPolicy{}
	if err := r.Get(ctx, req.NamespacedName, psp); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.ensureDefaultPolicy(ctx); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	if err := security.SyncPlatformSecurityConfigMap(ctx, r.Client, psp.Spec.AllowedMacWaivers); err != nil {
		setPSPCondition(psp, conditionPlatformSecurityReady, metav1.ConditionFalse, "SyncFailed", err.Error())
		_ = r.Status().Update(ctx, psp)
		return ctrl.Result{RequeueAfter: platformSecurityRequeue}, err
	}

	psp.Status.AllowedMacWaiverCount = len(psp.Spec.AllowedMacWaivers)
	setPSPCondition(psp, conditionPlatformSecurityReady, metav1.ConditionTrue, "Synced",
		"Platform security ConfigMap updated")
	if err := r.Status().Update(ctx, psp); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("platform security policy synced", "waivers", psp.Status.AllowedMacWaiverCount)
	return ctrl.Result{RequeueAfter: platformSecurityRequeue}, nil
}

func (r *PlatformSecurityPolicyReconciler) ensureDefaultPolicy(ctx context.Context) error {
	psp := &gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec:       gentianov1alpha1.PlatformSecurityPolicySpec{},
	}
	if err := r.Create(ctx, psp); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create default PlatformSecurityPolicy: %w", err)
	}
	return nil
}

func (r *PlatformSecurityPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.PlatformSecurityPolicy{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

func setPSPCondition(psp *gentianov1alpha1.PlatformSecurityPolicy, typ string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range psp.Status.Conditions {
		if psp.Status.Conditions[i].Type == typ {
			psp.Status.Conditions[i].Status = status
			psp.Status.Conditions[i].Reason = reason
			psp.Status.Conditions[i].Message = message
			psp.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	psp.Status.Conditions = append(psp.Status.Conditions, metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// LoadPlatformSecurityPolicy is a test helper accessor.
func LoadPlatformSecurityPolicy(ctx context.Context, c client.Client) (*gentianov1alpha1.PlatformSecurityPolicy, error) {
	psp := &gentianov1alpha1.PlatformSecurityPolicy{}
	err := c.Get(ctx, types.NamespacedName{Name: gentianov1alpha1.PlatformSecurityPolicyName}, psp)
	return psp, err
}
