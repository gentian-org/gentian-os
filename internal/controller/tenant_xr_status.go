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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const conditionCrossplaneReady = "CrossplaneReady"

func tenantHasConditionTrue(tenant *gentianov1alpha1.Tenant, condType string) bool {
	for _, c := range tenant.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// tenantFoundationConditions are the conditions a Tenant must carry, all True,
// before it can be Ready: the identity and data-plane stages.
//
// Exactly the stages the phase calculation already consults through their
// RequeueAfter, and deliberately not AppsReady, MailReady or
// AppPrivilegesReady — finalize logs "tenant ready; apps still converging" and
// returns their result, so those three are understood to settle after Ready and
// must not hold it back.
//
// Every one of them is set on every path its stage takes, including the
// not-applicable path, which reports True with a reason saying so:
// NoStorageRequired, PortalShellReady, "No apps require provisioning". So an
// absent condition here means the stage has not run, never that it had nothing
// to do.
var tenantFoundationConditions = []string{
	conditionIdentityReady,
	conditionDatabaseReady,
	conditionMariaDBReady,
	conditionStorageReady,
	conditionCacheReady,
}

// tenantFoundationNotReady returns the first foundation condition that is absent
// or not True, or "" when all of them are True.
//
// The phase used to be decided by whether any stage asked to be requeued, which
// is a different question. A Tenant reconciled before its AppProfiles resolve
// runs the data-plane stage with nothing yet to do, asks for no requeue, and
// finalizes — so it read Ready while carrying AppsReady=False and with all five
// of the conditions above never set at all.
//
// Absent counts as not ready, and that is the whole point: the conditions were
// missing rather than False, so a check for "no False condition" would have
// called that tenant Ready too.
func tenantFoundationNotReady(tenant *gentianov1alpha1.Tenant) string {
	for _, want := range tenantFoundationConditions {
		satisfied := false
		for i := range tenant.Status.Conditions {
			c := tenant.Status.Conditions[i]
			if c.Type != want {
				continue
			}
			satisfied = c.Status == metav1.ConditionTrue
			break
		}
		if !satisfied {
			return want
		}
	}
	return ""
}

// aggregateCrossplaneStatus maps XTenant readiness onto Tenant.status.conditions so
// operators can correlate Tenant Ready with the Crossplane composite graph.
func (r *TenantReconciler) aggregateCrossplaneStatus(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xTenantGVK)
	err := r.Get(ctx, types.NamespacedName{Name: tenant.Name}, xr)
	if errors.IsNotFound(err) {
		r.setCondition(tenant, conditionCrossplaneReady, metav1.ConditionFalse, "XRNotFound",
			"XTenant composite not yet created")
		return nil
	}
	if err != nil {
		return fmt.Errorf("get XTenant %s: %w", tenant.Name, err)
	}

	ready, reason, message := xTenantReadyCondition(xr)
	if ready {
		r.setCondition(tenant, conditionCrossplaneReady, metav1.ConditionTrue, "Ready", message)
		return nil
	}
	if reason == "" {
		reason = "NotReady"
	}
	if message == "" {
		message = "XTenant composite is not Ready"
	}
	r.setCondition(tenant, conditionCrossplaneReady, metav1.ConditionFalse, reason, message)
	return nil
}

func xTenantReadyCondition(xr *unstructured.Unstructured) (ready bool, reason, message string) {
	conditions, found, err := unstructured.NestedSlice(xr.Object, "status", "conditions")
	if err != nil || !found {
		return false, "StatusUnknown", "XTenant status conditions not yet reported"
	}

	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := cond["type"].(string)
		if typ != "Ready" {
			continue
		}
		status, _ := cond["status"].(string)
		reason, _ = cond["reason"].(string)
		message, _ = cond["message"].(string)
		return status == string(metav1.ConditionTrue), reason, message
	}
	return false, "ReadyConditionMissing", "XTenant has no Ready condition"
}

// deleteTenantProvisioningConfigMap removes the manifest bridge ConfigMap once the
// tenant is fully deleted from the cluster.
func (r *TenantReconciler) deleteTenantProvisioningConfigMap(ctx context.Context, tenantName string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantProvisioningConfigMapName(tenantName),
			Namespace: kernelNamespace,
		},
	}
	return client.IgnoreNotFound(r.Delete(ctx, cm))
}
