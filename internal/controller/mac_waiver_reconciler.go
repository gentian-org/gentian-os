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
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/security"
)

const (
	approvedMacWaiversAnnotation = "gentianos.io/approved-mac-waivers"
	conditionMacWaiversReady     = "MacWaiversReady"
)

// ensureMacWaivers intersects AppProfile requests with PlatformSecurityPolicy allowlist
// and records approved waivers on the Tenant for compositions to consume.
func (r *TenantReconciler) ensureMacWaivers(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	allowed, err := security.LoadAllowedMacWaivers(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	approvedByProfile := map[string][]gentianov1alpha1.MacWaiverRequest{}
	pendingDenials := []string{}

	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: profileName}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s for mac waivers: %w", profileName, err)
		}
		if profile.Spec.Security == nil || len(profile.Spec.Security.MacWaivers) == 0 {
			continue
		}
		approved := security.ApprovedMacWaivers(profileName, profile.Spec.Security.MacWaivers, allowed)
		if len(approved) > 0 {
			approvedByProfile[profileName] = approved
		}
		for _, req := range profile.Spec.Security.MacWaivers {
			if !security.IsWaiverApproved(approved, req.Policy, req.Scope) {
				pendingDenials = append(pendingDenials,
					fmt.Sprintf("%s/%s/%s", profileName, req.Policy, req.Scope))
			}
		}
	}

	payload, err := json.Marshal(approvedByProfile)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal approved mac waivers: %w", err)
	}
	encoded := string(payload)
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	if tenant.Annotations[approvedMacWaiversAnnotation] != encoded {
		patch := client.MergeFrom(tenant.DeepCopy())
		tenant.Annotations[approvedMacWaiversAnnotation] = encoded
		if err := r.Patch(ctx, tenant, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch tenant mac waiver annotations: %w", err)
		}
	}

	if len(pendingDenials) > 0 {
		r.setCondition(tenant, conditionMacWaiversReady, metav1.ConditionFalse, "WaiverNotApproved",
			fmt.Sprintf("MAC waivers pending cluster approval: %v", pendingDenials))
	} else {
		r.setCondition(tenant, conditionMacWaiversReady, metav1.ConditionTrue, "Approved",
			"All requested MAC waivers are approved or none requested")
	}
	return ctrl.Result{}, nil
}
