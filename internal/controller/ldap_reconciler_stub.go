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

// ensureLDAP is a no-op on the Keycloak-native (Suze) path.
// Full LDAP provisioning lives in ldap_reconciler_legacy.go (build tag legacy_ldap).
func (r *TenantReconciler) ensureLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	r.setCondition(tenant, conditionLDAPReady, metav1.ConditionTrue,
		"SkippedKeycloakNative", "LDAP provisioning disabled; Keycloak is authoritative")
	return ctrl.Result{}, nil
}

// deleteLDAP is a no-op on the Keycloak-native path (no UDM/LDAP resources).
func (r *TenantReconciler) deleteLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	_ = ctx
	_ = tenant
	return nil
}

// mbaGroupsJobName is referenced when cleaning up legacy LDAP Jobs from older clusters.
func mbaGroupsJobName(tenantName string) string {
	return fmt.Sprintf("ldap-mba-groups-%s", tenantName)
}
