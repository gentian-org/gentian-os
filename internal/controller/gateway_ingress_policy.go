/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	exposure *gentianov1alpha1.ExposureSpec,
) (gatewayFrameAncestorsPolicy, bool, error) {
	spec, err := gentianov1alpha1.GatewayFrameAncestors(exposure.Annotations)
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
	seen := make(map[string]struct{})
	var origins []string
	add := func(origin string) {
		if origin == "" {
			return
		}
		if _, dup := seen[origin]; dup {
			return
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	for _, token := range spec.Origins {
		switch strings.TrimSpace(token) {
		case gatewayFrameAncestorsOriginPortal:
			for _, origin := range consoleOrigins(kernelDomain, effectiveDomain) {
				add(origin)
			}
		case gatewayFrameAncestorsOriginMainApp:
			if effectiveDomain != "" && mainIngressSubDomain != "" {
				if mainIngressSubDomain == "@" {
					add(fmt.Sprintf("https://%s", effectiveDomain))
				} else {
					add(fmt.Sprintf("https://%s.%s", mainIngressSubDomain, effectiveDomain))
				}
			}
		default:
			add(strings.ReplaceAll(token, "${TENANT_DOMAIN}", effectiveDomain))
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

func ingressNeedsEscapedSlashesKeepUnchanged(exposure *gentianov1alpha1.ExposureSpec) bool {
	return gentianov1alpha1.GatewayEscapedSlashesAction(exposure.Annotations) == "KeepUnchanged"
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
		// The FIRST gateway exposure is what spec.ingress was: the app's own
		// host. Any further gateway entry is what additionalIngresses were.
		gateways := profile.GatewayExposures()
		if len(gateways) > 0 {
			intents = append(intents, ingressIntent{appProfile: app.Profile, profile: profile, ingress: gateways[0]})
		}
		intents = append(intents, additionalIngressIntents(app.Profile, profile)...)

		// Addons too. An addon is not a separate install and has no App claim,
		// but it can still need a hostname: Odoo's website module publishes the
		// tenant's homepage, which belongs on its own subdomain rather than on
		// the ERP host its base answers. Walking only app.Profile meant an
		// ingress declared on an addon was read by nobody -- no route, no
		// certificate, no DNS record, and no error to say so.
		//
		// Only AdditionalIngresses, never Spec.Ingress. An addon is reached
		// inside its base and does not get a primary host of its own; what it
		// may do is add a hostname, pointed at whichever Service it names.
		for _, addonProfile := range app.Addons {
			addon, ok := appProfileFromIndex(profileIndex, addonProfile)
			if !ok {
				continue
			}
			intents = append(intents, additionalIngressIntents(addonProfile, addon)...)
		}
	}
	return intents, nil
}

// additionalIngressIntents turns a profile's AdditionalIngresses into intents.
// The name carries the profile the ingress was declared on, so an addon's host
// is named for the addon rather than for the base it is activated inside.
func additionalIngressIntents(
	profileName string, profile *gentianov1alpha1.ComponentProfile,
) []ingressIntent {
	gateways := profile.GatewayExposures()
	if len(gateways) > 0 {
		gateways = gateways[1:] // the first is the app's own host, above
	}
	intents := make([]ingressIntent, 0, len(gateways))
	for i := range gateways {
		intents = append(intents, ingressIntent{
			appProfile: additionalIngressProfile(profileName, i),
			profile:    profile,
			ingress:    gateways[i],
		})
	}
	return intents
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
