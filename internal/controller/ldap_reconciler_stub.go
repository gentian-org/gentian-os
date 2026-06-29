//go:build !legacy_ldap

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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const conditionLDAPReady = "LDAPReady"

// ensureLDAP is the historical reconcile hook; Suze provisions per-tenant Keycloak realms only.
func (r *TenantReconciler) ensureLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	r.setCondition(tenant, conditionLDAPReady, metav1.ConditionTrue,
		"SuzeKeycloakNative", "Tenant identity is authoritative in the Keycloak realm")
	return ctrl.Result{}, nil
}

// deleteLDAP is handled by deleteIdentity (Keycloak realm delete/disable Jobs).
func (r *TenantReconciler) deleteLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	_ = ctx
	_ = tenant
	return nil
}

// mbaGroupsJobName names a legacy provisioning Job label; deleteIdentity still
// removes matching kernel Jobs when present on upgraded clusters.
func mbaGroupsJobName(tenantName string) string {
	return fmt.Sprintf("ldap-mba-groups-%s", tenantName)
}
