// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
)

// ensureKeycloakIDPEmbeddingIngress patches the shared Keycloak ingress so
// portal-embedded tenant apps (chat.<tenant>.<kernel>, etc.) may frame OIDC pages.
func (r *TenantReconciler) ensureKeycloakIDPEmbeddingIngress(ctx context.Context) error {
	return reconcileKeycloakIDPEmbeddingIngress(ctx, r.Client, r.KernelDomain, r.TenancyMode)
}
