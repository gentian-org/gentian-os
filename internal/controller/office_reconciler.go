/*
Copyright 2026 The Gentian Authors.

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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	conditionOfficeReady = "OfficeReady"
)

// ensureOffice sets the OfficeReady condition on the tenant based on
// spec.office.enabled. Document editing is provided by the shared kernel
// Collabora service — no per-tenant provisioning is required. The WOPI
// backend URL is configured statically in the nextcloud-management kernel
// service values and applied on every Nextcloud pod startup.
func (r *TenantReconciler) ensureOffice(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if tenant.Spec.Office == nil || !tenant.Spec.Office.Enabled {
		r.setCondition(tenant, conditionOfficeReady, metav1.ConditionTrue,
			"Disabled", "Nextcloud Office is not enabled for this tenant")
		return ctrl.Result{}, nil
	}

	r.setCondition(tenant, conditionOfficeReady, metav1.ConditionTrue,
		"Enabled", "Nextcloud Office is served by the shared kernel Collabora service")
	return ctrl.Result{}, nil
}
