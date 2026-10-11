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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/modelgateway"
)

// The model gateway for a component the platform places on tenants itself:
// the desktop, as the chart ships its profile.

func shippedDesktopProfile(t *testing.T) *gentianov1alpha1.ComponentProfile {
	return renderShippedProfile(t, "componentprofile-desktop.yaml",
		"desktop.enabled=true", "desktop.chart.version=0.1.0-develop.0000000")
}

// placedComponent is the Component ensureComponent makes for a profile the
// platform places.
func placedComponent(tenant, name string) *gentianov1alpha1.Component {
	return &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "tenant-" + tenant,
			Labels: map[string]string{tenantLabel: tenant, managedByLabel: managedByValue, componentOriginLabel: componentOriginDefault},
		},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: name},
			Class:      gentianov1alpha1.ComponentClassApp,
		},
	}
}

// newPlacedWorld is tenant demo with the desktop placed on it and no app.
func newPlacedWorld(t *testing.T, objs ...client.Object) *modelAccessWorld {
	t.Helper()
	w := newModelAccessWorld(t, nil, append(objs, placedComponent("demo", "desktop"))...)
	w.profiles["desktop"] = shippedDesktopProfile(t)
	return w
}

func (w *modelAccessWorld) verdict(t *testing.T, name string) modelAccessVerdict {
	t.Helper()
	comp := &gentianov1alpha1.Component{}
	if err := w.r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: tenantNamespaceName(w.tenant)}, comp); err != nil {
		t.Fatal(err)
	}
	v, err := modelAccessFor(context.Background(), w.r.Client, comp, w.profiles[name], w.tenant)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The profile the chart ships declares the gateway as the desktop's chart
// reads it: optional, of platform trust, the Secret by name and never the
// key, and rendered by the Component reconciler rather than the Composition.
func TestTheDesktopProfileDeclaresTheModelGatewayOptionally(t *testing.T) {
	p := shippedDesktopProfile(t)
	if p.Spec.TrustTier != gentianov1alpha1.TrustTierPlatform || !p.Spec.DefaultForTenants {
		t.Fatalf("tier %s, defaultForTenants %v", p.Spec.TrustTier, p.Spec.DefaultForTenants)
	}
	llm := p.Services().LLM
	if llm == nil || !llm.Optional {
		t.Fatalf("requires.services.llm = %+v, want it declared and optional", llm)
	}
	m := p.Spec.Package.ValueMapping.LLM
	if m == nil || m.AvailableKey != "llm.available" || m.BaseURLKey != "llm.baseUrl" || m.SecretNameKey != "llm.apiKeySecretName" || m.APIKeyKey != "" {
		t.Fatalf("valueMapping.llm = %+v", m)
	}
	if composedDelivery(p) {
		t.Error("the desktop is handed to the app Composition")
	}
	if refusal := placedModelAccessRefusal(p); refusal != "" {
		t.Errorf("refused: %s", refusal)
	}
}

// A placed component that declares the gateway is served as an app is: a key
// of its own under the tenant's and the component's names, in the vault, at
// the gateway and in its Secret, and on the tenant's record. Its chart is
// told the address and the Secret's name; the key is in no value. The way to
// the gateway is one port of one namespace.
func TestAPlacedComponentThatDeclaresTheModelGatewayIsServed(t *testing.T) {
	w := newPlacedWorld(t)
	state := w.pass(t)
	if len(state.served) != 1 || state.served[0] != "desktop" || len(state.waiting) != 0 {
		t.Fatalf("state = %+v", state)
	}
	secret := w.secret(t, "desktop")
	if secret == nil {
		t.Fatal("no Secret llm-credentials-desktop")
	}
	key := secretValue(secret, modelCredentialsAPIKey)
	if !strings.HasPrefix(key, modelgateway.KeyPrefix) || key == modelgateway.LegacyKey("demo", "desktop") {
		t.Fatalf("key = %q", key)
	}
	if w.gateway.keys["demo-desktop"] != modelgateway.HashKey(key) || len(w.gateway.keys) != 1 {
		t.Errorf("gateway keys = %v", w.gateway.keys)
	}
	if held := w.vault[secrets.CategoryPath("demo", "desktop", secrets.ModelAccessCategory)]; held["api-key"] != key {
		t.Errorf("the vault does not hold the key under the component's own path: %v", w.vault)
	}
	recorded, err := w.r.provisionedStores(context.Background(), "demo")
	if err != nil || recorded["desktop"].ModelKey != "demo-desktop" {
		t.Errorf("record = %+v, %v", recorded, err)
	}

	v := w.verdict(t, "desktop")
	if v.reason != "" || !v.placed || !v.delivered {
		t.Fatalf("verdict = %+v", v)
	}
	comp := placedComponent("demo", "desktop")
	values := modelAccessValues(comp, w.profiles["desktop"], v.delivered)
	llm, _ := values["llm"].(map[string]interface{})
	if llm["available"] != true || llm["apiKeySecretName"] != "llm-credentials-desktop" ||
		llm["baseUrl"] != modelgateway.OpenAIBaseURL(litellmProxyBaseURL) {
		t.Errorf("values = %v", values)
	}
	raw, _ := json.Marshal(values)
	if strings.Contains(string(raw), key) {
		t.Error("the key is among the release values")
	}

	np := buildComponentNetworkPolicy(comp, nil, []networkingv1.NetworkPolicyEgressRule{modelGatewayEgress()})
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("egress = %+v", np.Spec.Egress)
	}
	rule := np.Spec.Egress[0]
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != layout.System("llm") ||
		rule.To[0].PodSelector != nil || rule.To[0].IPBlock != nil {
		t.Errorf("peer = %+v", rule.To)
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != provisioner.ModelGatewayPort || *rule.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("ports = %+v", rule.Ports)
	}

	// A second pass changes nothing.
	w.pass(t)
	if got := secretValue(w.secret(t, "desktop"), modelCredentialsAPIKey); got != key {
		t.Error("the key changed on a second pass")
	}
}

// Optional, and the cluster serves no models: nothing is made, nothing holds
// the component, and its chart is told there is no gateway. The same while a
// gateway that should be there has not delivered the key. Not optional, the
// component waits as an app does.
func TestAnOptionalRequirementReleasesThePlacedComponentWithoutTheGateway(t *testing.T) {
	w := newPlacedWorld(t)
	t.Setenv("LLM_SUPPORT", "false")
	state := w.pass(t)
	if len(state.served) != 0 || w.secret(t, "desktop") != nil || len(w.gateway.keys) != 0 || len(w.vault) != 0 {
		t.Fatalf("something was made on a cluster with no gateway: %+v", state)
	}
	recorded, _ := w.r.provisionedStores(context.Background(), "demo")
	if recorded["desktop"].ModelKey != "" {
		t.Error("a key is on record that was never registered")
	}
	v := w.verdict(t, "desktop")
	if v.reason != "" || !v.placed || v.delivered {
		t.Fatalf("no gateway: verdict = %+v", v)
	}
	values := modelAccessValues(placedComponent("demo", "desktop"), w.profiles["desktop"], v.delivered)
	llm, _ := values["llm"].(map[string]interface{})
	if llm["available"] != false || llm["baseUrl"] != "" || llm["apiKeySecretName"] != "" {
		t.Errorf("values = %v, want the chart told there is no gateway", values)
	}

	// A gateway that does not answer: still released, still without.
	t.Setenv("LLM_SUPPORT", "true")
	w.gateway.down = true
	w.pass(t)
	if v := w.verdict(t, "desktop"); v.reason != "" || v.delivered {
		t.Errorf("gateway down: verdict = %+v", v)
	}
	// A Secret of the name that is not the operator's is not taken for it.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: modelCredentialsSecretName("desktop"), Namespace: "tenant-demo"},
		Data:       map[string][]byte{modelCredentialsAPIKey: []byte("sk-somebody-elses")},
	}
	if err := w.r.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if v := w.verdict(t, "desktop"); v.reason != "" || v.delivered {
		t.Errorf("a foreign Secret: verdict = %+v", v)
	}
	if err := w.r.Delete(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}

	// The same profile without optional holds, and says which requirement.
	required := w.profiles["desktop"].DeepCopy()
	required.Spec.Requires.Services.LLM.Optional = false
	comp := placedComponent("demo", "desktop")
	hold, err := modelAccessFor(context.Background(), w.r.Client, comp, required, w.tenant)
	if err != nil || hold.reason != "ModelAccessPending" {
		t.Errorf("required, gateway down: %+v %v", hold, err)
	}
	t.Setenv("LLM_SUPPORT", "false")
	hold, err = modelAccessFor(context.Background(), w.r.Client, comp, required, w.tenant)
	if err != nil || hold.reason != "ModelGatewayUnavailable" {
		t.Errorf("required, no gateway: %+v %v", hold, err)
	}

	// The gateway arrives: the key is delivered and the verdict says so.
	t.Setenv("LLM_SUPPORT", "true")
	w.gateway.down = false
	w.pass(t)
	if v := w.verdict(t, "desktop"); v.reason != "" || !v.delivered {
		t.Errorf("served: verdict = %+v", v)
	}
}

// Only a profile of platform trust is served as a placed component, and none
// that asks for the key as a chart value: neither is given a key, and the
// Component says why.
func TestAPlacedComponentIsServedOnlyAtPlatformTrustAndNeverByValue(t *testing.T) {
	for name, mutate := range map[string]func(*gentianov1alpha1.ComponentProfile){
		"a certified profile": func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
		},
		"the key as a chart value": func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Package.ValueMapping.LLM.APIKeyKey = "llm.apiKey"
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newPlacedWorld(t)
			mutate(w.profiles["desktop"])
			state := w.pass(t)
			if len(state.served) != 0 || w.secret(t, "desktop") != nil || len(w.gateway.keys) != 0 || len(w.vault) != 0 {
				t.Fatalf("it was given a key: %+v", state)
			}
			v := w.verdict(t, "desktop")
			if v.reason != "ModelAccessUnsupported" || v.placed || v.delivered {
				t.Fatalf("verdict = %+v", v)
			}
		})
	}
}

// A component the tenant has by another way -- no placement label -- is not
// served by the placed route, whatever its profile declares.
func TestOnlyAComponentThePlatformPlacedIsServedAsOne(t *testing.T) {
	w := newPlacedWorld(t)
	comp := placedComponent("demo", "desktop")
	delete(comp.Labels, componentOriginLabel)
	v, err := modelAccessFor(context.Background(), w.r.Client, comp, w.profiles["desktop"], w.tenant)
	if err != nil || v.reason != "ModelAccessUnsupported" || v.placed {
		t.Fatalf("verdict = %+v %v", v, err)
	}
}

// Each tenant's desktop has a key of its own: another alias, another vault
// path, another key.
func TestAnotherTenantsDesktopHasADifferentKey(t *testing.T) {
	w := newPlacedWorld(t, placedComponent("other", "desktop"))
	w.pass(t)
	first := secretValue(w.secret(t, "desktop"), modelCredentialsAPIKey)

	demo := w.tenant
	w.tenant = planTenant("other")
	w.pass(t)
	theirs := &corev1.Secret{}
	if err := w.r.Get(context.Background(), types.NamespacedName{Name: "llm-credentials-desktop", Namespace: "tenant-other"}, theirs); err != nil {
		t.Fatal(err)
	}
	second := secretValue(theirs, modelCredentialsAPIKey)
	if second == "" || second == first {
		t.Fatalf("the two tenants hold the same key, or none: %q %q", first, second)
	}
	if w.gateway.keys["demo-desktop"] != modelgateway.HashKey(first) || w.gateway.keys["other-desktop"] != modelgateway.HashKey(second) {
		t.Errorf("gateway keys = %v", w.gateway.keys)
	}
	if theirs.Labels[tenantLabel] != "other" {
		t.Errorf("labels = %v", theirs.Labels)
	}
	// And the first tenant's is untouched by the second's pass.
	w.tenant = demo
	if got := secretValue(w.secret(t, "desktop"), modelCredentialsAPIKey); got != first {
		t.Error("the first tenant's key changed")
	}
}

// What removes a placed component's key: the tenant deleted with its data,
// the profile no longer declaring the gateway, and the platform taking the
// component away. In each the key stops authenticating.
func TestWhatRemovesAPlacedComponentsKey(t *testing.T) {
	ctx := context.Background()

	t.Run("the tenant is deleted with its data", func(t *testing.T) {
		w := newPlacedWorld(t)
		w.pass(t)
		w.tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyRetain
		if err := w.r.deleteModelAccess(ctx, w.tenant); err != nil || len(w.gateway.keys) != 1 {
			t.Fatalf("deletionPolicy Retain: %v, keys = %v", err, w.gateway.keys)
		}
		w.tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
		if err := w.r.deleteModelAccess(ctx, w.tenant); err != nil {
			t.Fatal(err)
		}
		if len(w.gateway.keys) != 0 {
			t.Errorf("the desktop's key is still registered: %v", w.gateway.keys)
		}
	})

	t.Run("the profile stops declaring the gateway", func(t *testing.T) {
		w := newPlacedWorld(t)
		w.pass(t)
		w.profiles["desktop"].Spec.Requires.Services.LLM = nil
		state := w.pass(t)
		if len(state.removed) != 1 || state.removed[0] != "desktop" {
			t.Fatalf("state = %+v", state)
		}
		recorded, _ := w.r.provisionedStores(ctx, "demo")
		if w.secret(t, "desktop") != nil || len(w.gateway.keys) != 0 || recorded["desktop"].ModelKey != "" {
			t.Errorf("secret %v, keys %v, record %+v", w.secret(t, "desktop") != nil, w.gateway.keys, recorded)
		}
	})

	t.Run("the platform takes the component away", func(t *testing.T) {
		w := newPlacedWorld(t)
		w.pass(t)
		if err := w.r.Delete(ctx, placedComponent("demo", "desktop")); err != nil {
			t.Fatal(err)
		}
		state := w.pass(t)
		if len(state.removed) != 1 {
			t.Fatalf("state = %+v", state)
		}
		recorded, _ := w.r.provisionedStores(ctx, "demo")
		if w.secret(t, "desktop") != nil || len(w.gateway.keys) != 0 || recorded["desktop"].ModelKey != "" {
			t.Errorf("secret %v, keys %v, record %+v", w.secret(t, "desktop") != nil, w.gateway.keys, recorded)
		}
	})
}

// The apps beside a placed component are served as before, and an
// uninstalled app's generated key is still kept.
func TestAPlacedComponentDoesNotChangeWhatAppsAreGiven(t *testing.T) {
	w := newModelAccessWorld(t, []string{"chat", "wiki"}, placedComponent("demo", "desktop"))
	w.profiles["desktop"] = shippedDesktopProfile(t)
	state := w.pass(t)
	if len(state.served) != 2 || len(w.gateway.keys) != 2 || w.secret(t, "wiki") != nil {
		t.Fatalf("state = %+v, keys = %v", state, w.gateway.keys)
	}
	if a, b := secretValue(w.secret(t, "chat"), modelCredentialsAPIKey), secretValue(w.secret(t, "desktop"), modelCredentialsAPIKey); a == b {
		t.Error("the app and the desktop hold the same key")
	}
	w.tenant.Spec.Apps = w.tenant.Spec.Apps[1:]
	w.pass(t)
	if _, kept := w.gateway.keys["demo-chat"]; !kept {
		t.Error("an uninstalled app's generated key was removed")
	}
}
