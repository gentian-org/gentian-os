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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// AllowedCategories is the set of valid AppProfile categories.
var AllowedCategories = map[string]bool{
	"Office":        true,
	"Productivity":  true,
	"Collaboration": true,
	"Utility":       true,
	"Communication": true,
	"Finance":       true,
	"Development":   true,
	"Business":      true,
	"Design":        true,
	"Security":      true,
}

// AppProfileValidator validates AppProfile create/update requests.
//
// +kubebuilder:webhook:path=/validate-gentianos-io-v1alpha1-appprofile,mutating=false,failurePolicy=fail,sideEffects=None,groups=gentianos.io,resources=appprofiles,verbs=create;update,versions=v1alpha1,name=vappprofile.gentianos.io,admissionReviewVersions=v1
type AppProfileValidator struct {
	Client  client.Client
	Decoder admission.Decoder
}

// Handle implements admission.Handler.
func (v *AppProfileValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if v == nil {
		return admission.Errored(http.StatusInternalServerError, errors.New("appprofile validator is nil"))
	}

	ap := &gentianov1alpha1.AppProfile{}
	if v.Decoder != nil {
		if err := v.Decoder.DecodeRaw(req.Object, ap); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	} else {
		if err := json.Unmarshal(req.Object.Raw, ap); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}

	if err := v.Validate(ctx, ap); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

// Validate runs all validation checks and returns a human-readable error if any fail.
func (v *AppProfileValidator) Validate(ctx context.Context, ap *gentianov1alpha1.AppProfile) error {
	for _, cat := range ap.Spec.Categories {
		if !AllowedCategories[cat] {
			return fmt.Errorf("invalid category %q; must be one of Office, Productivity, Collaboration, Utility, Communication, Finance, Development, Business, Design, Security", cat)
		}
	}
	return nil
}

// InjectDecoder satisfies admission.DecoderInjector.
func (v *AppProfileValidator) InjectDecoder(d admission.Decoder) error {
	v.Decoder = d
	return nil
}

// SetupWithManager registers the webhook path with the controller-manager's webhook server.
func (v *AppProfileValidator) SetupWithManager(mgr ctrl.Manager) {
	mgr.GetWebhookServer().Register(
		"/validate-gentianos-io-v1alpha1-appprofile",
		&admission.Webhook{Handler: v},
	)
}
