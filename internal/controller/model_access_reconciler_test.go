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
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/modelgateway"
)

// keyGateway is the model gateway's key administration as it keeps keys: by
// alias, each as the hash of the key. It refuses a second key under an alias,
// as the gateway does.
type keyGateway struct {
	keys map[string]string // alias -> hash
	down bool
}

func (g *keyGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	var body struct {
		Key        string   `json:"key"`
		KeyAlias   string   `json:"key_alias"`
		KeyAliases []string `json:"key_aliases"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/key/list":
		keys := []string{}
		if hash, ok := g.keys[r.URL.Query().Get("key_alias")]; ok {
			keys = append(keys, hash)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	case "/key/generate":
		if _, taken := g.keys[body.KeyAlias]; taken {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		g.keys[body.KeyAlias] = modelgateway.HashKey(body.Key)
		_, _ = w.Write([]byte(`{}`))
	case "/key/delete":
		for _, alias := range body.KeyAliases {
			delete(g.keys, alias)
		}
		_, _ = w.Write([]byte(`{}`))
	case "/team/list":
		// No teams: the tests of the keys keep none.
		_, _ = w.Write([]byte(`[]`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func llmProfile(name string, derived ...string) *gentianov1alpha1.ComponentProfile {
	p := storesProfile(name, "", false, false)
	p.Spec.Requires.Services.LLM = &gentianov1alpha1.LLMRequirement{}
	p.Spec.Package.Chart = &gentianov1alpha1.ChartRef{Name: name}
	for _, key := range derived {
		if p.Spec.Secrets == nil {
			p.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{}
		}
		p.Spec.Secrets.Derived = append(p.Spec.Secrets.Derived, gentianov1alpha1.DerivedSecretKey{Key: key})
	}
	return p
}

func plainProfile(name string) *gentianov1alpha1.ComponentProfile {
	p := storesProfile(name, "", false, false)
	p.Spec.Package.Chart = &gentianov1alpha1.ChartRef{Name: name}
	return p
}

func gatewayAdminKey() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: litellmMasterKeySecret, Namespace: litellmMasterKeyNS},
		Data:       map[string][]byte{litellmMasterKeySecKey: []byte("sk-master")},
	}
}

// operatorModelSecret is the Secret an earlier version wrote for every app.
func operatorModelSecret(tenant, app, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: modelCredentialsSecretName(app), Namespace: "tenant-" + tenant,
			Labels: map[string]string{tenantLabel: tenant, managedByLabel: managedByValue, appLabel: app},
		},
		Data: map[string][]byte{modelCredentialsAPIKey: []byte(key)},
	}
}

type modelAccessWorld struct {
	r        *TenantReconciler
	gateway  *keyGateway
	vault    memVault
	tenant   *gentianov1alpha1.Tenant
	profiles map[string]*gentianov1alpha1.ComponentProfile
}

// newModelAccessWorld is tenant demo on a cluster that serves models, with
// the apps given: chat declares the gateway, every other name does not.
func newModelAccessWorld(t *testing.T, apps []string, objs ...client.Object) *modelAccessWorld {
	t.Helper()
	t.Setenv("LLM_SUPPORT", "true")
	w := &modelAccessWorld{
		gateway:  &keyGateway{keys: map[string]string{}},
		vault:    memVault{},
		tenant:   planTenant("demo", apps...),
		profiles: map[string]*gentianov1alpha1.ComponentProfile{},
	}
	withLiteLLM(t, w.gateway)
	for _, app := range append([]string{"chat", "wiki", "notes"}, apps...) {
		if _, ok := w.profiles[app]; ok {
			continue
		}
		if app == "chat" {
			w.profiles[app] = llmProfile(app, "WEBUI_SECRET_KEY")
		} else {
			w.profiles[app] = plainProfile(app)
		}
	}
	scheme := deleteGapsScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, gatewayAdminKey())...).Build()
	w.r = &TenantReconciler{Client: c, Scheme: scheme, Seeder: secrets.NewSeeder(w.vault, secrets.NewDeriver("unit-test-master"))}
	return w
}

func (w *modelAccessWorld) pass(t *testing.T) modelAccessState {
	t.Helper()
	ctx := context.Background()
	// As a pass of the tenant does: the record first, then the requirement.
	for name, p := range w.profiles {
		existing := &gentianov1alpha1.ComponentProfile{}
		if err := w.r.Get(ctx, types.NamespacedName{Name: name}, existing); errors.IsNotFound(err) {
			if err := w.r.Create(ctx, p.DeepCopy()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.r.recordProvisionedStores(ctx, w.tenant); err != nil {
		t.Fatal(err)
	}
	state, err := w.r.ensureModelAccess(ctx, w.tenant, w.profiles)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (w *modelAccessWorld) secret(t *testing.T, app string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	err := w.r.Get(context.Background(), types.NamespacedName{Name: modelCredentialsSecretName(app), Namespace: "tenant-demo"}, s)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func secretValue(s *corev1.Secret, key string) string {
	if v, ok := s.Data[key]; ok {
		return string(v)
	}
	return s.StringData[key]
}

// Only an app that declares the gateway is given anything: a key registered
// under its alias, and the Secret with the address and that key. The app
// beside it that declared nothing gets neither.
func TestOnlyAnAppThatDeclaresTheModelGatewayIsGivenAKey(t *testing.T) {
	w := newModelAccessWorld(t, []string{"chat", "wiki"})
	state := w.pass(t)
	if len(state.served) != 1 || state.served[0] != "chat" || len(state.waiting) != 0 {
		t.Fatalf("state = %+v", state)
	}

	s := w.secret(t, "chat")
	if s == nil {
		t.Fatal("the declaring app has no Secret")
	}
	key := secretValue(s, modelCredentialsAPIKey)
	if !strings.HasPrefix(key, modelgateway.KeyPrefix) || len(key) < 40 {
		t.Errorf("key = %q, want a generated key the gateway accepts", key)
	}
	// Nothing about the key follows from the two names.
	if key == modelgateway.LegacyKey("demo", "chat") || strings.Contains(key, "demo") || strings.Contains(key, "chat") {
		t.Errorf("key %q is made from the tenant's or the app's name", key)
	}
	want := litellmProxyBaseURL + "/v1"
	if secretValue(s, modelCredentialsBaseKey) != want || secretValue(s, modelCredentialsBaseURLKey) != want {
		t.Errorf("address = %q / %q, want %q", secretValue(s, modelCredentialsBaseKey), secretValue(s, modelCredentialsBaseURLKey), want)
	}
	// The key the profile declared is the one the vault holds for it.
	declared := w.vault[secrets.DerivedKeyPath("demo", "chat", "WEBUI_SECRET_KEY")]["value"]
	if declared == "" || secretValue(s, "WEBUI_SECRET_KEY") != declared {
		t.Errorf("WEBUI_SECRET_KEY = %q, the vault holds %q", secretValue(s, "WEBUI_SECRET_KEY"), declared)
	}
	if w.gateway.keys["demo-chat"] != modelgateway.HashKey(key) {
		t.Errorf("the gateway holds %v, want the Secret's key under demo-chat", w.gateway.keys)
	}
	// The vault holds the same key, where the app's other credentials are.
	if got := w.vault[secrets.CategoryPath("demo", "chat", secrets.ModelAccessCategory)]; got["api-key"] != key || got["base-url"] != want {
		t.Errorf("vault = %v", got)
	}

	if w.secret(t, "wiki") != nil {
		t.Error("an app that declared no gateway was given a Secret")
	}
	if _, there := w.gateway.keys["demo-wiki"]; there || len(w.gateway.keys) != 1 {
		t.Errorf("gateway keys = %v, want the declaring app's only", w.gateway.keys)
	}
	if _, there := w.vault[secrets.CategoryPath("demo", "wiki", secrets.ModelAccessCategory)]; there {
		t.Error("a key was made in the vault for an app that declared no gateway")
	}
	recorded, _ := w.r.provisionedStores(context.Background(), "demo")
	if recorded["chat"].ModelKey != "demo-chat" || recorded["wiki"].ModelKey != "" {
		t.Errorf("record = %+v", recorded)
	}
}

// The key is the app's for as long as the vault keeps it: a second pass
// changes nothing, and an app uninstalled and installed again -- its Secret
// gone with the namespace's cleanup, its registration removed -- holds the
// key it held. A purge deletes the vault's record and the gateway's key; what
// is installed after that is registered afresh.
func TestTheModelKeyIsStableAcrossAReinstallAndGoneAfterAPurge(t *testing.T) {
	ctx := context.Background()
	w := newModelAccessWorld(t, []string{"chat"})
	w.pass(t)
	first := secretValue(w.secret(t, "chat"), modelCredentialsAPIKey)

	w.pass(t)
	if got := secretValue(w.secret(t, "chat"), modelCredentialsAPIKey); got != first {
		t.Fatalf("a second pass changed the key")
	}

	// Uninstalled: the Secret goes with the app's records, the vault keeps.
	if err := w.r.Delete(ctx, w.secret(t, "chat")); err != nil {
		t.Fatal(err)
	}
	delete(w.gateway.keys, "demo-chat")
	w.pass(t)
	if got := secretValue(w.secret(t, "chat"), modelCredentialsAPIKey); got != first {
		t.Errorf("the app installed again holds another key")
	}
	if w.gateway.keys["demo-chat"] != modelgateway.HashKey(first) {
		t.Error("the key was not registered again")
	}

	// Purged: the credentials and the model access are destroyed by the
	// shared teardown (applifecycle); here, what that leaves behind.
	path := secrets.CategoryPath("demo", "chat", secrets.ModelAccessCategory)
	delete(w.vault, path)
	delete(w.vault, secrets.DerivedKeyPath("demo", "chat", "WEBUI_SECRET_KEY"))
	delete(w.gateway.keys, "demo-chat")
	if err := w.r.Delete(ctx, w.secret(t, "chat")); err != nil {
		t.Fatal(err)
	}
	if err := w.r.forgetModelAccess(ctx, "demo", "chat"); err != nil {
		t.Fatal(err)
	}
	w.tenant.Spec.Apps = nil
	if state := w.pass(t); len(state.served)+len(state.removed)+len(state.waiting) != 0 {
		t.Fatalf("a purged app was acted on: %+v", state)
	}
	if len(w.gateway.keys) != 0 || len(w.vault) != 0 || w.secret(t, "chat") != nil {
		t.Errorf("after the purge: gateway %v, vault %v", w.gateway.keys, w.vault)
	}
}

// An existing cluster: the declaring app holds the key made from names, in
// its Secret and at the gateway. The next pass replaces it in both, and the
// old key no longer authenticates.
func TestTheKeyMadeFromNamesIsReplaced(t *testing.T) {
	legacy := modelgateway.LegacyKey("demo", "chat")
	w := newModelAccessWorld(t, []string{"chat"}, operatorModelSecret("demo", "chat", legacy))
	w.gateway.keys["demo-chat"] = modelgateway.HashKey(legacy)
	w.gateway.keys["other-chat"] = modelgateway.HashKey(modelgateway.LegacyKey("other", "chat"))

	w.pass(t)
	key := secretValue(w.secret(t, "chat"), modelCredentialsAPIKey)
	if key == legacy || key == "" {
		t.Fatalf("the Secret still holds %q", key)
	}
	if w.gateway.keys["demo-chat"] != modelgateway.HashKey(key) {
		t.Error("the gateway does not hold the generated key under the app's alias")
	}
	for alias, hash := range w.gateway.keys {
		if alias != "other-chat" && hash == modelgateway.HashKey(legacy) {
			t.Errorf("the key made from names is still registered, as %s", alias)
		}
	}
	// Another tenant's key is that tenant's reconciler's to replace.
	if w.gateway.keys["other-chat"] != modelgateway.HashKey(modelgateway.LegacyKey("other", "chat")) {
		t.Error("another tenant's key was touched")
	}
}

// An existing cluster, the other way round: an app that never declared the
// gateway holds a Secret and a key from when every app was given one. Both
// are taken away, and the record says so. Nothing that is not that app's is
// touched: not another tenant's key, not a Secret of the same shape somebody
// else made, not the declaring app beside it.
func TestWhatAnUndeclaredAppWasGivenIsTakenAway(t *testing.T) {
	ctx := context.Background()
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: modelCredentialsSecretName("notes"), Namespace: "tenant-demo"},
		Data:       map[string][]byte{modelCredentialsAPIKey: []byte("sk-a-persons-own")},
	}
	record := backup.NewProvisionedRecord("demo")
	for _, app := range []string{"wiki", "notes", "chat"} {
		if _, err := backup.RecordProvisioned(record, app, backup.Provisioned{ModelKey: "demo-" + app, Bucket: "demo-" + app}); err != nil {
			t.Fatal(err)
		}
	}
	w := newModelAccessWorld(t, []string{"chat", "wiki", "notes"},
		operatorModelSecret("demo", "wiki", modelgateway.LegacyKey("demo", "wiki")), foreign, record)
	for _, alias := range []string{"demo-wiki", "demo-notes", "other-wiki"} {
		w.gateway.keys[alias] = modelgateway.HashKey("sk-gentian-" + alias)
	}

	state := w.pass(t)
	if len(state.removed) != 2 || len(state.served) != 1 {
		t.Fatalf("state = %+v", state)
	}
	if w.secret(t, "wiki") != nil {
		t.Error("the undeclared app still has its Secret")
	}
	if got := w.secret(t, "notes"); got == nil || secretValue(got, modelCredentialsAPIKey) != "sk-a-persons-own" {
		t.Error("a Secret the operator did not write was removed or changed")
	}
	if _, there := w.gateway.keys["demo-wiki"]; there {
		t.Error("the undeclared app's key is still registered")
	}
	if _, there := w.gateway.keys["demo-notes"]; there {
		t.Error("the key on record for the second undeclared app is still registered")
	}
	if _, there := w.gateway.keys["other-wiki"]; !there {
		t.Error("another tenant's key was removed")
	}
	if _, there := w.gateway.keys["demo-chat"]; !there {
		t.Error("the declaring app has no key")
	}
	recorded, err := w.r.provisionedStores(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if recorded["wiki"].ModelKey != "" || recorded["notes"].ModelKey != "" || recorded["chat"].ModelKey != "demo-chat" {
		t.Errorf("record = %+v", recorded)
	}
	// What else is on record for the app stays there.
	if recorded["wiki"].Bucket != "demo-wiki" {
		t.Errorf("the app's bucket left the record with its model key: %+v", recorded["wiki"])
	}
	// And a second pass has nothing left to take.
	if state := w.pass(t); len(state.removed) != 0 {
		t.Errorf("a second pass removed %v", state.removed)
	}
}

// An app that is uninstalled keeps its model access -- unless the key on
// record is still the one made from names, which nothing presents any more
// and anybody could: that one goes. A generated key of an uninstalled app
// stays until the app is purged.
func TestAnUninstalledAppsNamedKeyIsRemovedAndItsGeneratedKeyKept(t *testing.T) {
	record := backup.NewProvisionedRecord("demo")
	for _, app := range []string{"old", "kept"} {
		if _, err := backup.RecordProvisioned(record, app, backup.Provisioned{ModelKey: "demo-" + app}); err != nil {
			t.Fatal(err)
		}
	}
	w := newModelAccessWorld(t, nil, record)
	w.gateway.keys["demo-old"] = modelgateway.HashKey(modelgateway.LegacyKey("demo", "old"))
	w.gateway.keys["demo-kept"] = modelgateway.HashKey("sk-0123456789abcdef")

	state := w.pass(t)
	if len(state.removed) != 1 || state.removed[0] != "old" {
		t.Fatalf("state = %+v", state)
	}
	if _, there := w.gateway.keys["demo-old"]; there {
		t.Error("the named key of an uninstalled app is still registered")
	}
	if _, there := w.gateway.keys["demo-kept"]; !there {
		t.Error("the generated key of an uninstalled app was removed; uninstalling keeps it")
	}
	recorded, _ := w.r.provisionedStores(context.Background(), "demo")
	if recorded["old"].ModelKey != "" || recorded["kept"].ModelKey != "demo-kept" {
		t.Errorf("record = %+v", recorded)
	}
}

// A cluster with no gateway, and one whose gateway does not answer: the
// declaring app is given nothing and waits, the pass does not fail, and the
// app's Component says why it is held.
func TestADeclaringAppWaitsWhereThereIsNoGateway(t *testing.T) {
	ctx := context.Background()
	w := newModelAccessWorld(t, []string{"chat", "wiki"})
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "tenant-demo"}}

	t.Setenv("LLM_SUPPORT", "false")
	state := w.pass(t)
	if len(state.served) != 0 || len(state.waiting) != 1 || !strings.Contains(state.waiting[0], "no model gateway") {
		t.Fatalf("no gateway: state = %+v", state)
	}
	if w.secret(t, "chat") != nil || len(w.gateway.keys) != 0 || len(w.vault) != 0 {
		t.Error("something was made for an app on a cluster with no gateway")
	}
	reason, message, err := modelAccessHold(ctx, w.r.Client, comp, w.profiles["chat"], w.tenant)
	if err != nil || reason != "ModelGatewayUnavailable" || !strings.Contains(message, "this cluster has no model gateway") {
		t.Errorf("hold = %q %q %v", reason, message, err)
	}

	t.Setenv("LLM_SUPPORT", "true")
	w.gateway.down = true
	state = w.pass(t)
	if len(state.served) != 0 || len(state.waiting) != 1 {
		t.Fatalf("gateway down: state = %+v", state)
	}
	if w.secret(t, "chat") != nil {
		t.Error("a Secret was written with a key the gateway never registered")
	}
	reason, _, err = modelAccessHold(ctx, w.r.Client, comp, w.profiles["chat"], w.tenant)
	if err != nil || reason != "ModelAccessPending" {
		t.Errorf("hold = %q %v, want ModelAccessPending", reason, err)
	}

	w.gateway.down = false
	w.pass(t)
	if reason, message, err := modelAccessHold(ctx, w.r.Client, comp, w.profiles["chat"], w.tenant); err != nil || reason != "" {
		t.Errorf("served, and still held: %q %q %v", reason, message, err)
	}

	// An app that declared nothing is never held, whatever the cluster runs.
	plain := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-demo"}}
	t.Setenv("LLM_SUPPORT", "false")
	if reason, _, err := modelAccessHold(ctx, w.r.Client, plain, w.profiles["wiki"], w.tenant); err != nil || reason != "" {
		t.Errorf("an app that declared no gateway is held: %q %v", reason, err)
	}
	// And a component that is not one of the tenant's apps is told nothing
	// serves it, rather than waiting for ever on a Secret nobody writes.
	t.Setenv("LLM_SUPPORT", "true")
	stray := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "tenant-demo"}}
	if reason, _, err := modelAccessHold(ctx, w.r.Client, stray, llmProfile("console"), w.tenant); err != nil || reason != "ModelAccessUnsupported" {
		t.Errorf("hold = %q %v, want ModelAccessUnsupported", reason, err)
	}
}

// A gateway that does not answer keeps an undeclared app's key on record:
// it may still authenticate. The Secret, which nothing consumes, goes.
func TestAKeyThatCannotBeRemovedStaysOnRecord(t *testing.T) {
	record := backup.NewProvisionedRecord("demo")
	if _, err := backup.RecordProvisioned(record, "wiki", backup.Provisioned{ModelKey: "demo-wiki"}); err != nil {
		t.Fatal(err)
	}
	w := newModelAccessWorld(t, []string{"wiki"}, record,
		operatorModelSecret("demo", "wiki", modelgateway.LegacyKey("demo", "wiki")))
	w.gateway.keys["demo-wiki"] = modelgateway.HashKey(modelgateway.LegacyKey("demo", "wiki"))
	w.gateway.down = true

	state := w.pass(t)
	if len(state.removed) != 0 || len(state.waiting) != 1 {
		t.Fatalf("state = %+v", state)
	}
	if w.secret(t, "wiki") != nil {
		t.Error("the Secret is still there")
	}
	recorded, _ := w.r.provisionedStores(context.Background(), "demo")
	if recorded["wiki"].ModelKey != "demo-wiki" {
		t.Error("the key left the record while the gateway still holds it")
	}

	w.gateway.down = false
	if state := w.pass(t); len(state.removed) != 1 {
		t.Fatalf("state = %+v", state)
	}
	recorded, _ = w.r.provisionedStores(context.Background(), "demo")
	if recorded["wiki"].ModelKey != "" || len(w.gateway.keys) != 0 {
		t.Errorf("record = %+v, gateway = %v", recorded, w.gateway.keys)
	}
}

// Without a vault the key is still generated, and kept in the app's Secret.
func TestWithoutAVaultTheKeyIsRandomAndKeptInTheSecret(t *testing.T) {
	w := newModelAccessWorld(t, []string{"chat"}, operatorModelSecret("demo", "chat", modelgateway.LegacyKey("demo", "chat")))
	w.r.Seeder = nil
	w.pass(t)
	first := secretValue(w.secret(t, "chat"), modelCredentialsAPIKey)
	if first == modelgateway.LegacyKey("demo", "chat") || !strings.HasPrefix(first, modelgateway.KeyPrefix) {
		t.Fatalf("key = %q", first)
	}
	w.pass(t)
	if got := secretValue(w.secret(t, "chat"), modelCredentialsAPIKey); got != first {
		t.Error("the key changed on a second pass")
	}
	if w.gateway.keys["demo-chat"] != modelgateway.HashKey(first) {
		t.Error("the gateway does not hold the Secret's key")
	}
}
