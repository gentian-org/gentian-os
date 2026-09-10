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

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/handover"
	"github.com/gentian-org/gentian-os/internal/tenancy"
)

// TenantValidator validates Tenant create/update requests.
//
// Checks performed:
//  1. Every app in spec.apps references an AppProfile that exists.
//  2. The number of apps does not exceed spec.quotas.maxApps (when set).
//
// +kubebuilder:webhook:path=/validate-gentianos-io-v1alpha1-tenant,mutating=false,failurePolicy=fail,sideEffects=None,groups=gentianos.io,resources=tenants,verbs=create;update,versions=v1alpha1,name=vtenant.gentianos.io,admissionReviewVersions=v1
type TenantValidator struct {
	Client       client.Client
	Decoder      admission.Decoder
	TenancyMode  string
	KernelDomain string

	// GateOnHandover refuses to admit a NEW tenant until the cluster's human
	// write path has been proven. See internal/handover for why that is the
	// gate: re-initialising OpenBao is the recovery if the write path turns
	// out broken, and it is affordable on an empty cluster and ruinous on one
	// carrying tenants.
	GateOnHandover bool
	// HandoverNamespace holds the record. Empty disables the gate, which is
	// what a misconfigured operator should do — refusing every tenant because
	// a namespace name is unset would be a worse failure than not gating.
	HandoverNamespace string
}

// HandoverOverrideAnnotation admits a tenant before the write path is proven.
//
// The value is the reason, and it is required: an override with no reason is
// indistinguishable from a mistake six months later, and this one accepts a
// recovery that may cost every tenant on the cluster. It lives on the object so
// the decision is in Git next to what it applies to, rather than in a flag
// somebody set on a controller once.
const HandoverOverrideAnnotation = "gentianos.io/handover-override"

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

	// Creation only. An existing tenant must stay manageable: gating updates
	// would mean a cluster that has not finished handover cannot fix the very
	// tenant that is misconfigured, which turns a precaution into a trap.
	if req.Operation == admissionv1.Create {
		if err := v.validateHandover(ctx, tenant); err != nil {
			return admission.Denied(err.Error())
		}
	}

	if err := v.Validate(ctx, tenant); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

// validateHandover refuses a new tenant while the write path is unproven.
//
// The message is long on purpose. A denial an operator cannot act on gets
// worked around by disabling the webhook, which removes the AppProfile and
// tenancy checks with it.
func (v *TenantValidator) validateHandover(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if !v.GateOnHandover || v.HandoverNamespace == "" {
		return nil
	}
	if reason := tenant.Annotations[HandoverOverrideAnnotation]; reason != "" {
		return nil
	}

	state, err := handover.Read(ctx, v.Client, v.HandoverNamespace)
	if err != nil {
		// Could not tell. Admitting is the right direction: the gate exists to
		// keep a recovery cheap, not to be the thing that stops a cluster
		// working, and refusing every tenant because the API server hiccupped
		// would be a self-inflicted outage.
		return nil
	}
	if state.WritePathProven {
		return nil
	}

	return fmt.Errorf(
		"tenant %q: this cluster has not proven its administrator can write credentials, "+
			"so creating tenants is held back.\n"+
			"  Why: if the login path turns out not to work, the fix is re-initialising "+
			"OpenBao — cheap on an empty cluster, and a data-loss event on one with tenants.\n"+
			"  To clear it: sign in to the portal as the cluster administrator. The first "+
			"successful sign-in proves the path and lifts this automatically.\n"+
			"  To proceed anyway: annotate this Tenant with %s=\"<reason>\"",
		tenant.Name, HandoverOverrideAnnotation,
	)
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

	if err := tenancy.EnforceSingle(ctx, v.Client, v.TenancyMode, tenant); err != nil {
		return err
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
