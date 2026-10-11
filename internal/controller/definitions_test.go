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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/schemacheck"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
	"github.com/gentian-org/gentian-os/internal/schemacheck/schemachecktest"
)

// definitionsWorld is an operator's view of a cluster for these tests: the
// definitions the cluster serves, one tenant and one component in it, and
// the tenant's and the component's reconcilers behind the real check.
type definitionsWorld struct {
	t         *testing.T
	api       client.Client
	checker   *crdcheck.Checker
	events    *record.FakeRecorder
	tenant    reconcile.Reconciler
	component reconcile.Reconciler
}

var (
	heldTenantKey    = types.NamespacedName{Name: "demo"}
	heldComponentKey = types.NamespacedName{Namespace: "tenant-demo", Name: "wiki"}
)

// newDefinitionsWorld starts from a cluster serving these definitions; nil
// in the list is a definition the cluster does not have.
func newDefinitionsWorld(t *testing.T, served []client.Object) *definitionsWorld {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: heldTenantKey.Name}}
	component := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Namespace: heldComponentKey.Namespace, Name: heldComponentKey.Name},
		Spec: gentianov1alpha1.ComponentSpec{
			Class:      gentianov1alpha1.ComponentClassApp,
			ProfileRef: gentianov1alpha1.ProfileRef{Name: "wiki"},
		},
	}
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(served, tenant, component)...).
		WithStatusSubresource(tenant, component).Build()

	gate := schemacheck.NewGate(schemacheck.Embedded())
	events := record.NewFakeRecorder(64)
	held := &crdcheck.Holder{Gate: gate, Recorder: events}
	return &definitionsWorld{
		t: t, api: api, events: events,
		checker:   &crdcheck.Checker{Reader: api, Gate: gate},
		tenant:    (&TenantReconciler{Client: api, APIReader: api, Scheme: scheme, Definitions: held}).guarded(),
		component: (&ComponentReconciler{Client: api, Scheme: scheme, Definitions: held}).guarded(),
	}
}

func servedExcept(t *testing.T, replaced ...*apiextensionsv1.CustomResourceDefinition) []client.Object {
	t.Helper()
	by := map[string]*apiextensionsv1.CustomResourceDefinition{}
	for _, r := range replaced {
		by[r.Name] = r
	}
	var out []client.Object
	for _, obj := range schemachecktest.Current(schemacheck.Embedded()) {
		r, swapped := by[obj.GetName()]
		switch {
		case !swapped:
			out = append(out, obj)
		case len(r.Spec.Versions) > 0:
			out = append(out, r)
		}
	}
	return out
}

func notServed(name string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// serve brings one definition on the cluster up to date, as a sync or a
// re-run of B-06 does, and lets the operator's timer come round.
func (w *definitionsWorld) serve(kind string) {
	w.t.Helper()
	current := schemachecktest.Kind(w.t, schemacheck.Embedded(), kind)
	existing := &apiextensionsv1.CustomResourceDefinition{}
	if err := w.api.Get(context.Background(), types.NamespacedName{Name: current.Name}, existing); err != nil {
		if err := w.api.Create(context.Background(), current); err != nil {
			w.t.Fatal(err)
		}
		return
	}
	existing.Spec = current.Spec
	if err := w.api.Update(context.Background(), existing); err != nil {
		w.t.Fatal(err)
	}
}

func (w *definitionsWorld) reconcile(r reconcile.Reconciler, key types.NamespacedName) reconcile.Result {
	w.t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		w.t.Fatalf("reconcile %s: %v", key, err)
	}
	return res
}

// state reads back what the reconciler left: the hold condition, if any, and
// whether the reconciler itself ran -- the first thing either of these two
// does to an object is add its finalizer, so an object without one has not
// been reconciled at all.
func (w *definitionsWorld) state(obj client.Object, key types.NamespacedName, finalizer string) (*metav1.Condition, bool) {
	w.t.Helper()
	if err := w.api.Get(context.Background(), key, obj); err != nil {
		w.t.Fatal(err)
	}
	var conds []metav1.Condition
	switch o := obj.(type) {
	case *gentianov1alpha1.Tenant:
		conds = o.Status.Conditions
	case *gentianov1alpha1.Component:
		conds = o.Status.Conditions
	}
	return apimeta.FindStatusCondition(conds, schemacheck.ConditionType), controllerutil.ContainsFinalizer(obj, finalizer)
}

func (w *definitionsWorld) tenantState() (*metav1.Condition, bool) {
	return w.state(&gentianov1alpha1.Tenant{}, heldTenantKey, tenantFinalizer)
}

func (w *definitionsWorld) componentState() (*metav1.Condition, bool) {
	return w.state(&gentianov1alpha1.Component{}, heldComponentKey, componentFinalizer)
}

// expectHeld asserts an object carries the hold and was not reconciled.
func expectHeld(t *testing.T, what string, cond *metav1.Condition, ran bool, reason string, names ...string) {
	t.Helper()
	if ran {
		t.Fatalf("%s was reconciled while it should have been held", what)
	}
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reason {
		t.Fatalf("%s: want condition %s=False reason %s, got %+v", what, schemacheck.ConditionType, reason, cond)
	}
	for _, name := range names {
		if !strings.Contains(cond.Message, name) {
			t.Errorf("%s: the condition does not name %q: %s", what, name, cond.Message)
		}
	}
}

// Before the cluster has been read once, nothing is written: the reconcilers
// are already running when the first check completes, and "not checked yet"
// must not read as "in order".
func TestNothingIsReconciledBeforeTheDefinitionsWereCompared(t *testing.T) {
	w := newDefinitionsWorld(t, servedExcept(t))
	if res := w.reconcile(w.tenant, heldTenantKey); res.RequeueAfter <= 0 || res.RequeueAfter >= crdcheck.HoldRequeue {
		t.Fatalf("before the first check a reconcile must come back in a moment, got %+v", res)
	}
	// Not reconciled, and not marked either: this lasts a moment at every
	// start, and is not worth a write to every object.
	if cond, ran := w.tenantState(); cond != nil || ran {
		t.Fatalf("before the first check the tenant must be left alone: cond=%+v ran=%v", cond, ran)
	}
	if len(w.events.Events) != 0 {
		t.Fatal("an Event was recorded for an operator that has merely not checked yet")
	}

	w.checker.CheckOnce(context.Background())
	w.reconcile(w.tenant, heldTenantKey)
	if cond, ran := w.tenantState(); cond != nil || !ran {
		t.Fatalf("after the first check the tenant must be reconciled with the condition gone: cond=%+v ran=%v", cond, ran)
	}
}

// A CRD of the chart older than the operator: every reconciler of the
// operator's own kinds holds, says so on its object with the fields and the
// remedy, and resumes by itself once the chart's CRDs are synced.
func TestAnOutdatedChartDefinitionHoldsTheReconcilersUntilItIsSynced(t *testing.T) {
	set := schemacheck.Embedded()
	older := schemachecktest.Without(t, schemachecktest.Kind(t, set, "Tenant"), "spec.apps[].addonPins")
	w := newDefinitionsWorld(t, servedExcept(t, older))
	w.checker.CheckOnce(context.Background())

	for i := 0; i < 2; i++ { // twice: a held object stays held, and says it once
		if res := w.reconcile(w.tenant, heldTenantKey); res.RequeueAfter != crdcheck.HoldRequeue {
			t.Fatalf("held reconcile: %+v", res)
		}
		w.reconcile(w.component, heldComponentKey)
	}
	cond, ran := w.tenantState()
	expectHeld(t, "the tenant", cond, ran, schemacheck.ReasonOutdated,
		"Tenant", "tenants.gentianos.io", "spec.apps[].addonPins", "sync the gentian-os Application")
	// The component's reconciler writes chart kinds too, and the chart's
	// definitions arrive together: it holds as well.
	cond, ran = w.componentState()
	expectHeld(t, "the component", cond, ran, schemacheck.ReasonOutdated, "spec.apps[].addonPins")
	if got := len(w.events.Events); got != 2 {
		t.Errorf("want one Event per held object, got %d", got)
	}

	w.serve("Tenant")
	w.checker.CheckOnce(context.Background())
	// The pass that takes the condition off, then the reconcile proper.
	if res := w.reconcile(w.tenant, heldTenantKey); res.RequeueAfter == 0 || res.RequeueAfter == crdcheck.HoldRequeue {
		t.Fatalf("the releasing pass must come straight back, got %+v", res)
	}
	w.reconcile(w.tenant, heldTenantKey)
	if cond, ran := w.tenantState(); cond != nil || !ran {
		t.Fatalf("after the sync the tenant must be reconciled with the condition gone: cond=%+v ran=%v", cond, ran)
	}
	w.reconcile(w.component, heldComponentKey)
	w.reconcile(w.component, heldComponentKey)
	if cond, ran := w.componentState(); cond != nil || !ran {
		t.Fatalf("after the sync the component must be reconciled with the condition gone: cond=%+v ran=%v", cond, ran)
	}
}

// A CRD Crossplane generated from an older XRD: the reconciler that writes
// that kind holds and names installer step B-06; the one that does not write
// it carries on; and re-running the step lets the first resume.
func TestAnOutdatedGeneratedDefinitionHoldsItsWriterUntilB06IsReRun(t *testing.T) {
	set := schemacheck.Embedded()
	older := schemachecktest.Without(t, schemachecktest.Kind(t, set, "App"), "spec.pullSecrets")
	w := newDefinitionsWorld(t, servedExcept(t, older))
	w.checker.CheckOnce(context.Background())

	w.reconcile(w.component, heldComponentKey)
	cond, ran := w.componentState()
	expectHeld(t, "the component", cond, ran, schemacheck.ReasonOutdated,
		"App", "apps.gentianos.io", "spec.pullSecrets", "./install.sh --only B-06")

	w.reconcile(w.tenant, heldTenantKey)
	if cond, ran := w.tenantState(); cond != nil || !ran {
		t.Fatalf("the tenant reconciler does not write App claims and must not be held: cond=%+v ran=%v", cond, ran)
	}

	w.serve("App")
	w.checker.CheckOnce(context.Background())
	w.reconcile(w.component, heldComponentKey)
	w.reconcile(w.component, heldComponentKey)
	if cond, ran := w.componentState(); cond != nil || !ran {
		t.Fatalf("after B-06 the component must be reconciled with the condition gone: cond=%+v ran=%v", cond, ran)
	}
}

// The same for the tenant's own Crossplane kind.
func TestAnOutdatedXTenantHoldsTheTenantReconciler(t *testing.T) {
	set := schemacheck.Embedded()
	older := schemachecktest.Without(t, schemachecktest.Kind(t, set, "XTenant"), "spec.quotas")
	w := newDefinitionsWorld(t, servedExcept(t, older))
	w.checker.CheckOnce(context.Background())

	w.reconcile(w.tenant, heldTenantKey)
	cond, ran := w.tenantState()
	expectHeld(t, "the tenant", cond, ran, schemacheck.ReasonOutdated, "XTenant", "spec.quotas", "./install.sh --only B-06")

	w.reconcile(w.component, heldComponentKey)
	if cond, ran := w.componentState(); cond != nil || !ran {
		t.Fatalf("the component reconciler does not write XTenants and must not be held: cond=%+v ran=%v", cond, ran)
	}
}

// A fresh install. The installer applies the XRDs at B-06 and the operator
// arrives at D-01, so the generated CRDs are normally there; but Crossplane
// generates them on its own time, and an operator that starts before one
// exists must wait for it without calling it outdated -- and without making
// anything wait that does not write that kind.
func TestADefinitionNotGeneratedYetIsWaitedForNotCalledOutdated(t *testing.T) {
	w := newDefinitionsWorld(t, servedExcept(t, notServed("apps.gentianos.io"), notServed("xapps.gentianos.io")))
	w.checker.CheckOnce(context.Background())

	w.reconcile(w.component, heldComponentKey)
	cond, ran := w.componentState()
	expectHeld(t, "the component", cond, ran, schemacheck.ReasonNotReady, "not on the cluster yet", "Crossplane generates it")

	w.reconcile(w.tenant, heldTenantKey)
	if cond, ran := w.tenantState(); cond != nil || !ran {
		t.Fatalf("the tenant must not wait for a kind its reconciler does not write: cond=%+v ran=%v", cond, ran)
	}

	w.serve("App")
	w.serve("XApp")
	w.checker.CheckOnce(context.Background())
	w.reconcile(w.component, heldComponentKey)
	w.reconcile(w.component, heldComponentKey)
	if cond, ran := w.componentState(); cond != nil || !ran {
		t.Fatalf("once Crossplane generated the CRDs the component must be reconciled: cond=%+v ran=%v", cond, ran)
	}
}

// A kind no reconciler writes may be absent for as long as it likes.
func TestAnAbsentKindNothingWritesHoldsNothing(t *testing.T) {
	w := newDefinitionsWorld(t, servedExcept(t, notServed("suze.gentianos.io"), notServed("xsuze.gentianos.io")))
	w.checker.CheckOnce(context.Background())
	w.reconcile(w.tenant, heldTenantKey)
	w.reconcile(w.component, heldComponentKey)
	if cond, ran := w.tenantState(); cond != nil || !ran {
		t.Fatalf("tenant: cond=%+v ran=%v", cond, ran)
	}
	if cond, ran := w.componentState(); cond != nil || !ran {
		t.Fatalf("component: cond=%+v ran=%v", cond, ran)
	}
}

// operatorRules reads the chart's ClusterRole, which is generated from the
// rbac markers beside the code: what the operator is permitted to do is the
// one machine-readable statement of what it writes.
func operatorRules(t *testing.T) []struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "gentian-os", "templates", "clusterrole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, rules, ok := strings.Cut(string(raw), "\nrules:\n")
	if !ok {
		t.Fatal("the chart's ClusterRole has no rules")
	}
	var role struct {
		Rules []struct {
			APIGroups []string `json:"apiGroups"`
			Resources []string `json:"resources"`
			Verbs     []string `json:"verbs"`
		} `json:"rules"`
	}
	if err := yaml.Unmarshal([]byte("rules:\n"+rules), &role); err != nil {
		t.Fatal(err)
	}
	return role.Rules
}

// Every kind the operator may write is covered by the check.
//
// "May write" is read off the ClusterRole: any resource of this API group it
// can create, update or patch, status included. Each must have a definition
// embedded, or the operator writes a kind nothing compares. And each that
// Crossplane generates must be named by the reconciler that writes it,
// because those are not covered by "every definition of the chart" -- so a
// permission to write a new Crossplane kind fails here until its writer says
// so in generatedKindsWritten.
func TestEveryKindTheOperatorMayWriteIsChecked(t *testing.T) {
	set := schemacheck.Embedded()
	byCRD := map[string]schemacheck.Definition{}
	for _, def := range set.Definitions {
		byCRD[def.CRD] = def
	}
	named := map[string]bool{}
	for _, kinds := range generatedKindsWritten {
		for _, kind := range kinds {
			if _, ok := set.Kind(kind); !ok {
				t.Errorf("generatedKindsWritten names %s, which has no embedded definition", kind)
			}
			named[kind] = true
		}
	}

	writable := map[string]bool{}
	for _, rule := range operatorRules(t) {
		if len(rule.APIGroups) != 1 || rule.APIGroups[0] != schemacheck.Group {
			continue
		}
		writes := false
		for _, verb := range rule.Verbs {
			if verb == "create" || verb == "update" || verb == "patch" {
				writes = true
			}
		}
		if !writes {
			continue
		}
		for _, resource := range rule.Resources {
			base, sub, _ := strings.Cut(resource, "/")
			if sub == "finalizers" {
				continue // an owner reference permission, not a write of the kind
			}
			writable[base] = true
		}
	}
	if len(writable) == 0 {
		t.Fatal("the ClusterRole lets the operator write nothing in this API group: the test is reading the wrong thing")
	}
	var generated []string
	for resource := range writable {
		def, ok := byCRD[resource+"."+schemacheck.Group]
		if !ok {
			t.Errorf("the operator may write %s and no definition of it is embedded: nothing checks that the cluster serves its fields", resource)
			continue
		}
		if def.Source == schemacheck.SourceXRD {
			generated = append(generated, def.Kind)
			if !named[def.Kind] {
				t.Errorf("the operator may write %s, which Crossplane generates, and no reconciler names it: "+
					"add it to generatedKindsWritten for the reconciler that writes it", def.Kind)
			}
		}
	}
	sort.Strings(generated)
	for kind := range named {
		if i := sort.SearchStrings(generated, kind); i == len(generated) || generated[i] != kind {
			t.Errorf("generatedKindsWritten names %s, which the operator is not permitted to write", kind)
		}
	}
}

// Every reconciler of one of the operator's kinds runs behind the check, or
// is one known to write none of them.
//
// A reconciler is put behind it where it is registered, by handing the
// builder the guarded form. One registered bare would write through whatever
// the cluster serves; this reads the registrations and fails on a bare one
// that is not listed here with the reason it may be.
func TestEveryReconcilerThatWritesIsBehindTheCheck(t *testing.T) {
	// Reconcilers that write no object of this API group: they project what
	// they read into something else.
	writesNothingOfOurs := map[string]string{
		"authz_projection_reconciler.go":  "writes the authorization graph",
		"tile_projection_reconciler.go":   "writes a ConfigMap",
		"keycloak_platform_reconciler.go": "writes Keycloak's own resources",
		"gateway_platform_reconciler.go":  "writes Gateway API resources",
		"concierge_lookup.go":             "writes a ConfigMap",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	complete := regexp.MustCompile(`\bComplete\(([^)]*)`)
	seen := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range complete.FindAllStringSubmatch(string(raw), -1) {
			seen++
			arg := m[1]
			guarded := strings.Contains(arg, ".Definitions.Guard") || strings.Contains(arg, ".guarded")
			_, exempt := writesNothingOfOurs[file]
			switch {
			case guarded && exempt:
				t.Errorf("%s is behind the check and also listed as writing nothing: take it off the list", file)
			case !guarded && !exempt:
				t.Errorf("%s registers a reconciler with Complete(%s): hand it r.Definitions.Guard(...) so it is held while the "+
					"cluster's definitions would drop fields, or list it here with what it writes instead", file, arg)
			}
		}
	}
	if seen < 10 {
		t.Fatalf("found %d reconciler registrations; the pattern no longer matches how they are written", seen)
	}
}
