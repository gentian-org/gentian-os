/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// placeholderText names every token anything here expands.
const placeholderText = "d=${TENANT_DOMAIN} n=${TENANT_NAMESPACE} t=${TENANT_ID} a=${APP_ID} s=${SIDECAR_ID} k=${KERNEL_DOMAIN} i=${NODE_IP}"

// What each expander makes of every token, with and without the cluster's
// facts, held to testdata/placeholders.txt.
func TestPlaceholderTranscript(t *testing.T) {
	var out []string
	for _, cluster := range []bool{false, true} {
		in := &Input{App: "wiki", Namespace: "tenant-demo", TenantName: "demo", Domain: "demo.example", Profile: &gentianov1alpha1.ComponentProfileSpec{}}
		if cluster {
			in.KernelServices = map[string]string{"KERNEL_DOMAIN": "kernel.example"}
			in.ClusterConfig = map[string]string{"node.ip": "10.0.0.1"}
		}
		label := fmt.Sprintf("cluster=%v", cluster)
		raw, _ := json.Marshal(map[string]string{"v": placeholderText, placeholderText: "key"})

		in.Profile.Package.ExtraValues = &runtime.RawExtension{Raw: raw}
		in.Profile.Package.Chart = &gentianov1alpha1.ChartRef{}
		r := &valuesRender{in: in, spec: in.Profile, tenant: in.Tenant(), ns: in.Namespace, vm: &gentianov1alpha1.ValueMapping{}}
		out = append(out, label+" substitute: "+r.substitute(placeholderText))
		vals, err := r.inlineValues()
		out = append(out, fmt.Sprintf("%s inline values: %v %v", label, jsonOf(t, vals), err))

		ext := &gentianov1alpha1.AppSidecarSpec{Name: "meet", ExtraValues: &runtime.RawExtension{Raw: raw}}
		ev, err := extensionValues(in, ext)
		out = append(out, fmt.Sprintf("%s extension values: %v %v", label, jsonOf(t, ev), err))

		id := newIdentityScope(in)
		out = append(out, label+" identity place: "+id.place(placeholderText))

		in.Profile.Hooks = &gentianov1alpha1.HookSpec{PostInstall: &gentianov1alpha1.AppPostInstallJob{Image: "image", Script: placeholderText}}
		rendered := &Rendered{}
		err = renderHooks(in, rendered)
		for _, obj := range rendered.Objects {
			out = append(out, fmt.Sprintf("%s hooks %s %s: %v", label, obj.GetKind(), obj.GetName(), jsonOf(t, obj.Object)))
		}
		out = append(out, fmt.Sprintf("%s hooks: %v", label, err))
	}
	sort.Strings(out)
	got := strings.Join(out, "\n") + "\n"
	const path = "testdata/placeholders.txt"
	if os.Getenv("UPDATE_TRANSCRIPTS") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // A test fixture.
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // A test fixture.
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("the transcript differs from %s\n--- got\n%s--- want\n%s", path, got, want)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
