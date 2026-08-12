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
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	gatewayFrameAncestorsOriginPortal  = "portal"
	gatewayFrameAncestorsOriginMainApp = "mainApp"
)

func ingressFrameAncestorsPolicy(
	kernelDomain, effectiveDomain, mainIngressSubDomain string,
	ingress *gentianov1alpha1.IngressSpec,
) (gatewayFrameAncestorsPolicy, bool, error) {
	spec, err := gentianov1alpha1.IngressGatewayFrameAncestors(ingress)
	if err != nil {
		return gatewayFrameAncestorsPolicy{}, false, fmt.Errorf("parse %s: %w", gentianov1alpha1.AnnotationIngressGatewayFrameAncestors, err)
	}
	if spec == nil || len(spec.Origins) == 0 {
		return gatewayFrameAncestorsPolicy{}, false, nil
	}
	mode := strings.TrimSpace(spec.Mode)
	if mode == "" {
		mode = gatewayFrameAncestorsReplace
	}
	var origins []string
	for _, token := range spec.Origins {
		switch strings.TrimSpace(token) {
		case gatewayFrameAncestorsOriginPortal:
			if kernelDomain != "" {
				origins = append(origins, fmt.Sprintf("https://portal.%s", kernelDomain))
			}
		case gatewayFrameAncestorsOriginMainApp:
			if effectiveDomain != "" && mainIngressSubDomain != "" {
				if mainIngressSubDomain == "@" {
					origins = append(origins, fmt.Sprintf("https://%s", effectiveDomain))
				} else {
					origins = append(origins, fmt.Sprintf("https://%s.%s", mainIngressSubDomain, effectiveDomain))
				}
			}
		default:
			expanded := strings.ReplaceAll(token, "${TENANT_DOMAIN}", effectiveDomain)
			if expanded != "" {
				origins = append(origins, expanded)
			}
		}
	}
	if len(origins) == 0 {
		return gatewayFrameAncestorsPolicy{}, false, nil
	}
	return gatewayFrameAncestorsPolicy{
		Mode:    mode,
		Origins: strings.Join(origins, " "),
	}, true, nil
}

func ingressNeedsEscapedSlashesKeepUnchanged(ingress *gentianov1alpha1.IngressSpec) bool {
	return gentianov1alpha1.IngressGatewayEscapedSlashesAction(ingress) == "KeepUnchanged"
}

func anyIntentNeedsEscapedSlashesKeepUnchanged(intents []ingressIntent) bool {
	for _, intent := range intents {
		if ingressNeedsEscapedSlashesKeepUnchanged(intent.ingress) {
			return true
		}
	}
	return false
}

func collectTenantIngressIntents(ctx context.Context, c client.Client, tenant *gentianov1alpha1.Tenant) ([]ingressIntent, error) {
	profileIndex, err := loadAppProfileIndex(ctx, c)
	if err != nil {
		return nil, err
	}
	var intents []ingressIntent
	for _, app := range tenant.Spec.Apps {
		profile, ok := appProfileFromIndex(profileIndex, app.Profile)
		if !ok {
			continue
		}
		if profile.Spec.Ingress != nil {
			intents = append(intents, ingressIntent{appProfile: app.Profile, profile: profile, ingress: profile.Spec.Ingress})
		}
		for i := range profile.Spec.AdditionalIngresses {
			intents = append(intents, ingressIntent{
				appProfile: additionalIngressProfile(app.Profile, i),
				profile:    profile,
				ingress:    &profile.Spec.AdditionalIngresses[i],
			})
		}
	}
	return intents, nil
}

func clusterNeedsEscapedSlashesKeepUnchanged(ctx context.Context, c client.Client) (bool, error) {
	tenants := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenants); err != nil {
		return false, err
	}
	for i := range tenants.Items {
		intents, err := collectTenantIngressIntents(ctx, c, &tenants.Items[i])
		if err != nil {
			return false, err
		}
		if anyIntentNeedsEscapedSlashesKeepUnchanged(intents) {
			return true, nil
		}
	}
	return false, nil
}
