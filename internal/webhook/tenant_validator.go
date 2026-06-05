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

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TenantValidator validates Tenant create/update requests.
//
// Checks performed:
//  1. Every app in spec.apps references an AppProfile that exists.
//  2. The number of apps does not exceed spec.quotas.maxApps (when set).
//
// +kubebuilder:webhook:path=/validate-gentianos-io-v1alpha1-tenant,mutating=false,failurePolicy=fail,sideEffects=None,groups=gentianos.io,resources=tenants,verbs=create;update,versions=v1alpha1,name=vtenant.gentianos.io,admissionReviewVersions=v1
type TenantValidator struct {
	Client      client.Client
	Decoder     admission.Decoder
	TenancyMode string
	KernelDomain string
}

// Handle implements admission.Handler.
func (v *TenantValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if v == nil {
		return admission.Errored(http.StatusInternalServerError, errors.New("tenant validator is nil"))
	}
	if v.Client == nil {
		return admission.Errored(http.StatusInternalServerError, errors.New("tenant validator client is not initialized"))
	}

	tenant := &gentianov1alpha1.Tenant{}
	if v.Decoder != nil {
		if err := v.Decoder.DecodeRaw(req.Object, tenant); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	} else {
		if err := json.Unmarshal(req.Object.Raw, tenant); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}

	if err := v.Validate(ctx, tenant); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

// Validate runs all validation checks and returns a human-readable error if any fail.
// Exported so tests can exercise the same code path the webhook does.
func (v *TenantValidator) Validate(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	// Check maxApps quota.
	if tenant.Spec.Quotas != nil && tenant.Spec.Quotas.MaxApps > 0 {
		if int32(len(tenant.Spec.Apps)) > tenant.Spec.Quotas.MaxApps {
			return fmt.Errorf(
				"tenant %q: app count %d exceeds maxApps quota %d",
				tenant.Name, len(tenant.Spec.Apps), tenant.Spec.Quotas.MaxApps,
			)
		}
	}

	// Check each referenced AppProfile exists.
	seen := make(map[string]struct{}, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		if _, dup := seen[app.Profile]; dup {
			return fmt.Errorf("tenant %q: duplicate app profile %q in spec.apps", tenant.Name, app.Profile)
		}
		seen[app.Profile] = struct{}{}

		profile := &gentianov1alpha1.AppProfile{}
		err := v.Client.Get(ctx, types.NamespacedName{Name: app.Profile}, profile)
		if k8serrors.IsNotFound(err) {
			return fmt.Errorf(
				"tenant %q: AppProfile %q not found; install the app catalogue first",
				tenant.Name, app.Profile,
			)
		}
		if err != nil {
			return fmt.Errorf("tenant %q: looking up AppProfile %q: %w", tenant.Name, app.Profile, err)
		}
	}

	if err := v.validateTenancy(ctx, tenant); err != nil {
		return err
	}

	return nil
}

func (v *TenantValidator) validateTenancy(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	mode := gentianov1alpha1.NormalizeTenancyMode(v.TenancyMode)
	if mode != gentianov1alpha1.TenancyModeSingle {
		return nil
	}
	if tenant.Name != gentianov1alpha1.SingleTenantName {
		return fmt.Errorf(
			"cluster TENANCY_MODE=single allows only Tenant %q (got %q)",
			gentianov1alpha1.SingleTenantName, tenant.Name,
		)
	}
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.LDAPOu != "" &&
		tenant.Spec.Isolation.LDAPOu != gentianov1alpha1.SingleTenantLDAPOU {
		return fmt.Errorf(
			"cluster TENANCY_MODE=single requires spec.isolation.ldapOU %q (got %q)",
			gentianov1alpha1.SingleTenantLDAPOU, tenant.Spec.Isolation.LDAPOu,
		)
	}
	var others gentianov1alpha1.TenantList
	if err := v.Client.List(ctx, &others); err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	for i := range others.Items {
		other := &others.Items[i]
		if other.Name == tenant.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		return fmt.Errorf(
			"cluster TENANCY_MODE=single allows only one Tenant CR (found %q and %q)",
			tenant.Name, other.Name,
		)
	}
	return nil
}

// InjectDecoder satisfies admission.DecoderInjector so controller-runtime
// can inject the decoder automatically when the webhook is registered.
func (v *TenantValidator) InjectDecoder(d admission.Decoder) error {
	v.Decoder = d
	return nil
}

// SetupWithManager registers the webhook path with the controller-manager's
// webhook server. The webhook server itself (TLS certs, port) is configured
// in main.go via ctrl.Options.WebhookServer.
func (v *TenantValidator) SetupWithManager(mgr ctrl.Manager) {
	mgr.GetWebhookServer().Register(
		"/validate-gentianos-io-v1alpha1-tenant",
		&admission.Webhook{Handler: v},
	)
}
