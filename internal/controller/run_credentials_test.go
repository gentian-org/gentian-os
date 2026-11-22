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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// asking gives an action's profile one credential of two fields.
func asking(profile *gentianov1alpha1.ComponentProfile) {
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Supplied: []gentianov1alpha1.SuppliedSecret{{
		Name: "cloud", DisplayName: "Cloud account", Description: "A token of the cloud account.",
		Fields: []gentianov1alpha1.SuppliedField{
			{Key: "token", ValuePath: "CLOUD_TOKEN", Format: "token"},
			{Key: "account", ValuePath: "CLOUD_ACCOUNT", Public: true},
		},
	}}}
}

func externalSecret(t *testing.T, r *RunsReconciler, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, es)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// An action that asks a person for a credential is asked for it as an app
// is, while it is not Ready yet. Once it is Ready its credentials are one
// Secret in the tenant's namespace for runs, read through the tenant's own
// store, a file per credential, and the table names that Secret for the
// action and for no other. An action that is gone leaves nothing.
func TestAnActionsCredentialsReachItsRunsAsFiles(t *testing.T) {
	ctx := context.Background()
	flow := rightsComponent("acme", "flow")
	cloud, cloudProfile := actionFixture("acme", "cloud", true)
	asking(cloudProfile)
	cloud.Status.Conditions[0].Status = metav1.ConditionFalse
	ocr, ocrProfile := actionFixture("acme", "ocr", true)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "flow", Namespace: "tenant-acme"}, Spec: corev1.ServiceSpec{
		Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("http")}},
	}}
	c, r := runsHarness(t, rightsTenant("acme"), flow, runsProfile("flow"), cloud, cloudProfile, ocr, ocrProfile, service)
	if err := (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureRuns(ctx, flow, "acme", runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, runsRequest); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()

	// Asked for, and waited for: no run of it, and no Secret yet.
	requirement := &gentianov1alpha1.CredentialRequirement{}
	if err := c.Get(ctx, types.NamespacedName{Name: "app-acme-cloud-cloud"}, requirement); err != nil {
		t.Fatalf("the requirement: %v", err)
	}
	if requirement.Spec.Tenant != "acme" || requirement.Spec.Scope != "tenant" ||
		requirement.Spec.VaultPath != "gentian-os/tenants/acme/apps/cloud/supplied/cloud" ||
		len(requirement.Spec.Fields) != 2 || !requirement.Spec.Fields[0].Secret || requirement.Spec.Fields[1].Secret ||
		requirement.Labels["gentianos.io/credential-provider"] != "cloud" || requirement.Labels[runCredentialsLabel] != "cloud" ||
		!strings.Contains(requirement.Spec.Description, "No run of the action cloud starts") {
		t.Fatalf("requirement = %+v, labels %v", requirement.Spec, requirement.Labels)
	}
	if probe := externalSecret(t, r, meta.OperatorNamespace, "credreq-app-acme-cloud-cloud"); probe == nil {
		t.Fatal("the requirement has no probe where the custodian looks for one")
	}
	table := runsTable(t, c)
	caller := &table.Callers[0]
	if _, _, found := table.Runnable(caller, "cloud"); found {
		t.Fatal("an action that waits for its credentials can be run")
	}
	if externalSecret(t, r, "tenant-acme-runs", "run-credentials-cloud") != nil {
		t.Fatal("the credentials of an action that is not Ready were delivered")
	}

	// Ready: the Secret is asked for where its runs are, and named.
	cloud.Status.Conditions[0].Status = metav1.ConditionTrue
	if err := c.Status().Update(ctx, cloud); err != nil {
		t.Fatal(err)
	}
	reconcile()
	table = runsTable(t, c)
	caller = &table.Callers[0]
	action, _, found := table.Runnable(caller, "cloud")
	if !found || action.Credentials != "run-credentials-cloud" {
		t.Fatalf("cloud = %+v found=%v", action, found)
	}
	if other, _, _ := table.Runnable(caller, "ocr"); other.Credentials != "" {
		t.Fatalf("an action that asks for nothing is given %q", other.Credentials)
	}
	es := externalSecret(t, r, "tenant-acme-runs", "run-credentials-cloud")
	if es == nil {
		t.Fatal("no ExternalSecret in the namespace for runs")
	}
	store, _, _ := unstructured.NestedString(es.Object, "spec", "secretStoreRef", "name")
	target, _, _ := unstructured.NestedString(es.Object, "spec", "target", "name")
	files, _, _ := unstructured.NestedStringMap(es.Object, "spec", "target", "template", "data")
	if store != secrets.TenantStore("acme") || target != "run-credentials-cloud" || len(files) != 1 {
		t.Fatalf("store %q, target %q, files %v", store, target, files)
	}
	want := "CLOUD_TOKEN='{{ .c0f0 | replace \"'\" \"'\\\\''\" }}'\nCLOUD_ACCOUNT='{{ .c0f1 | replace \"'\" \"'\\\\''\" }}'\n"
	if files["cloud.env"] != want {
		t.Fatalf("cloud.env =\n%s\nwant\n%s", files["cloud.env"], want)
	}
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	first, _ := data[0].(map[string]any)
	ref, _ := first["remoteRef"].(map[string]any)
	if len(data) != 2 || first["secretKey"] != "c0f0" || ref["key"] != "gentian-os/tenants/acme/apps/cloud/supplied/cloud" || ref["property"] != "token" {
		t.Fatalf("data = %v", data)
	}

	// Removed: what was asked and what was delivered go with it.
	if err := c.Delete(ctx, cloud); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if err := c.Get(ctx, types.NamespacedName{Name: "app-acme-cloud-cloud"}, requirement); !errors.IsNotFound(err) {
		t.Fatalf("the requirement of an action that is gone: %v", err)
	}
	if externalSecret(t, r, meta.OperatorNamespace, "credreq-app-acme-cloud-cloud") != nil ||
		externalSecret(t, r, "tenant-acme-runs", "run-credentials-cloud") != nil {
		t.Fatal("the probe or the credentials of an action that is gone are still there")
	}
}

// For an action a field's valuePath is a variable's name in the credential's
// file, once per file.
func TestAnActionsCredentialFieldsNameVariables(t *testing.T) {
	_, profile := actionFixture("acme", "cloud", true)
	asking(profile)
	if refusal := actionCredentialsRefusal(profile); refusal != "" {
		t.Fatalf("refused: %s", refusal)
	}
	fields := profile.Spec.Secrets.Supplied[0].Fields
	fields[1].ValuePath = "CLOUD_TOKEN"
	if refusal := actionCredentialsRefusal(profile); !strings.Contains(refusal, "twice") {
		t.Fatalf("a variable named twice: %q", refusal)
	}
	for _, path := range []string{"cloud.token", "1TOKEN", "A B", "A=B", "$(id)"} {
		fields[1].ValuePath = path
		if refusal := actionCredentialsRefusal(profile); !strings.Contains(refusal, "valuePath") {
			t.Errorf("%q: %q", path, refusal)
		}
	}
}

const cloudBundle = stepBundle + `  secrets:
    supplied:
    - name: cloud
      displayName: Cloud account
      fields:
      - key: token
        valuePath: CLOUD_TOKEN
`

// An installed action whose profile asks a person for a credential is not
// Ready until it is set, and says what it waits for; one whose field names
// no variable is refused.
func TestAnActionWaitsForItsCredentials(t *testing.T) {
	ctx := context.Background()
	h := startDigestHarness(t, materialised(t, cloudBundle), "")
	h.comp.Spec.Class = gentianov1alpha1.ComponentClassAction
	if err := h.c.Update(ctx, h.comp); err != nil {
		t.Fatal(err)
	}
	ready := componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "False" || ready.Reason != "CredentialsPending" ||
		!strings.Contains(ready.Message, "Cloud account (cloud)") || !strings.Contains(ready.Message, "No run of it starts") {
		t.Fatalf("condition = %+v, want CredentialsPending", ready)
	}

	// Set: the requirement's probe reads Ready.
	tenant := acmeTenantFixture()
	probe := secrets.ExternalSecret("credreq-"+secrets.SuppliedRequirementName(tenant.Name, h.comp.Name, "cloud"),
		meta.OperatorNamespace, secrets.KernelStore, map[string]any{"creationPolicy": "None"}, nil)
	probe.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	if err := h.c.Create(ctx, probe); err != nil {
		t.Fatal(err)
	}
	if ready = componentReadyCondition(h.reconcile()); ready == nil || ready.Status != "True" {
		t.Fatalf("condition = %+v, want Ready", ready)
	}

	h = startDigestHarness(t, materialised(t, strings.Replace(cloudBundle, "CLOUD_TOKEN", "cloud.token", 1)), "")
	h.comp.Spec.Class = gentianov1alpha1.ComponentClassAction
	if err := h.c.Update(ctx, h.comp); err != nil {
		t.Fatal(err)
	}
	if ready = componentReadyCondition(h.reconcile()); ready == nil || ready.Status != "False" || ready.Reason != "ProfileInvalid" {
		t.Fatalf("condition = %+v, want ProfileInvalid", ready)
	}
}
