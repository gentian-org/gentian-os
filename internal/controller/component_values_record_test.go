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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What a component's config and the operator's values make of the render's
// input and of the App claim, held to testdata/component-values.txt.
func TestComponentValuesTranscript(t *testing.T) {
	ctx := context.Background()
	three := int32(3)
	configs := map[string]*gentianov1alpha1.TenantAppConfig{
		"none":          nil,
		"empty":         {},
		"replicas":      {Replicas: &three},
		"values":        {ExtraValues: &runtime.RawExtension{Raw: []byte(`{"a":{"b":1,"c":"own"},"n":2}`)}},
		"both":          {Replicas: &three, ExtraValues: &runtime.RawExtension{Raw: []byte(`{"a":{"c":"own"}}`)}},
		"empty values":  {ExtraValues: &runtime.RawExtension{Raw: []byte(`{}`)}},
		"not an object": {ExtraValues: &runtime.RawExtension{Raw: []byte(`[1]`)}},
	}
	operators := map[string]map[string]interface{}{
		"no operator values": nil,
		"operator values":    {"a": map[string]interface{}{"c": "operator", "d": int32(4)}, "e": 5},
	}
	var out []string
	for cname, cfg := range configs {
		for oname, ov := range operators {
			for _, placed := range []bool{false, true} {
				scheme := runtime.NewScheme()
				_ = gentianov1alpha1.AddToScheme(scheme)
				_ = corev1.AddToScheme(scheme)
				profile := chartProfileFixture("oci://registry.vendor.example/acme/charts")
				if placed {
					profile.Spec.Placement = &gentianov1alpha1.ComponentPlacement{}
				}
				c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile).Build()
				r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
				tenant := acmeTenantFixture()
				comp := componentFor("vendor-app", tenantNamespaceName(tenant))
				comp.Spec.ProfileRef.Name = profile.Name
				comp.UID = "uid-comp"
				if cfg != nil {
					comp.Spec.Config = cfg.DeepCopy()
				}
				label := fmt.Sprintf("%s, %s, placed=%v", cname, oname, placed)
				var opv map[string]interface{}
				if ov != nil {
					raw, _ := json.Marshal(ov)
					_ = json.Unmarshal(raw, &opv)
					opv["a"].(map[string]interface{})["d"] = int32(4)
				}
				in, err := r.renderInput(ctx, comp, profile, tenant, r.zoneOf(tenant), pullSecrets{}, false, nil, opv)
				if err != nil {
					out = append(out, fmt.Sprintf("%s: render input: %v", label, err))
				} else {
					replicas := "nil"
					if in.Replicas != nil {
						replicas = fmt.Sprint(*in.Replicas)
					}
					raw, _ := json.Marshal(in.ExtraValues)
					out = append(out, fmt.Sprintf("%s: render input: replicas=%s values=%s %T", label, replicas, raw, in.ExtraValues["e"]))
				}
				if placed {
					continue
				}
				if ov != nil {
					raw, _ := json.Marshal(ov)
					opv = nil
					_ = json.Unmarshal(raw, &opv)
					opv["a"].(map[string]interface{})["d"] = int32(4)
				}
				zone := edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}
				if _, _, err := r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}, "", "", false, nil, opv); err != nil {
					out = append(out, fmt.Sprintf("%s: claim: %v", label, err))
					continue
				}
				claim := &unstructured.Unstructured{}
				claim.SetGroupVersionKind(appClaimGVK)
				if err := c.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim); err != nil {
					t.Fatal(err)
				}
				config, has, _ := unstructured.NestedFieldNoCopy(claim.Object, "spec", "config")
				raw, _ := json.Marshal(config)
				out = append(out, fmt.Sprintf("%s: claim: has=%v config=%s", label, has, raw))
			}
		}
	}
	sort.Strings(out)
	holdToTranscript(t, *updateTranscripts, "component-values", strings.Join(out, "\n")+"\n")
}
