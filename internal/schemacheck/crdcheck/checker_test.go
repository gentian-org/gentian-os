/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package crdcheck_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
	"github.com/gentian-org/gentian-os/internal/schemacheck/schemachecktest"
)

// cluster is an API server holding these definitions, whose answer for any
// one of them can be replaced by an error.
type cluster struct {
	client.Client
	fail map[string]error
}

func newCluster(t *testing.T, crds ...client.Object) *cluster {
	t.Helper()
	c := &cluster{fail: map[string]error{}}
	c.Client = fake.NewClientBuilder().WithScheme(schemachecktest.Scheme(t)).WithObjects(crds...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.fail[key.Name]; err != nil {
					return err
				}
				return inner.Get(ctx, key, obj, opts...)
			},
		}).Build()
	return c
}

// replace swaps the definition the cluster holds for another.
func (c *cluster) replace(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) {
	t.Helper()
	existing := &apiextensionsv1.CustomResourceDefinition{}
	err := c.Get(context.Background(), client.ObjectKey{Name: crd.Name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		fresh := crd.DeepCopy()
		fresh.ResourceVersion = ""
		if err := c.Create(context.Background(), fresh); err != nil {
			t.Fatal(err)
		}
	case err != nil:
		t.Fatal(err)
	default:
		existing.Spec = *crd.Spec.DeepCopy()
		if err := c.Update(context.Background(), existing); err != nil {
			t.Fatal(err)
		}
	}
}

// without returns a current cluster's definitions with one kind's replaced.
func without(t *testing.T, set *schemacheck.Set, replaced ...*apiextensionsv1.CustomResourceDefinition) []client.Object {
	t.Helper()
	by := map[string]*apiextensionsv1.CustomResourceDefinition{}
	for _, r := range replaced {
		by[r.Name] = r
	}
	var out []client.Object
	for _, obj := range schemachecktest.Current(set) {
		if r, ok := by[obj.GetName()]; ok {
			if r.Spec.Versions != nil {
				out = append(out, r)
			}
			continue
		}
		out = append(out, obj)
	}
	return out
}

// absent marks a definition as one the cluster does not hold at all.
func absent(name string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	crd.Name = name
	return crd
}

func newChecker(c client.Reader) (*crdcheck.Checker, *schemacheck.Gate, *record.FakeRecorder) {
	set := schemacheck.Embedded()
	gate := schemacheck.NewGate(set)
	recorder := record.NewFakeRecorder(256)
	return &crdcheck.Checker{
		Reader: c, Gate: gate, Recorder: recorder,
		EventOn: &apiextensionsv1.CustomResourceDefinition{},
	}, gate, recorder
}

func kindOf(t *testing.T, r schemacheck.Report, kind string) schemacheck.KindReport {
	t.Helper()
	k, ok := r.Kind(kind)
	if !ok {
		t.Fatalf("the report says nothing about %s", kind)
	}
	return k
}

func TestBeforeTheFirstCheckEverythingIsHeld(t *testing.T) {
	gate := schemacheck.NewGate(schemacheck.Embedded())
	hold := gate.Hold(schemacheck.Chart)
	if hold == nil || hold.Reason != schemacheck.ReasonUnconfirmed {
		t.Fatalf("an unchecked gate must hold as unconfirmed, got %+v", hold)
	}
	var none *schemacheck.Gate
	if none.Hold(schemacheck.Chart) != nil {
		t.Fatal("a process that runs no check must hold nothing")
	}
}

func TestACurrentClusterIsReportedInOrder(t *testing.T) {
	set := schemacheck.Embedded()
	checker, gate, _ := newChecker(newCluster(t, schemachecktest.Current(set)...))
	report := checker.CheckOnce(context.Background())
	if !report.OK || !report.Checked || report.Definitions != set.Digest || len(report.Kinds) != len(set.Definitions) {
		t.Fatalf("a current cluster: %+v", report)
	}
	if hold := gate.Hold(schemacheck.Chart, schemacheck.Kinds(set, "App", "XTenant")); hold != nil {
		t.Fatalf("a current cluster holds nothing, got %+v", hold)
	}
}

func TestAnOutdatedChartDefinitionIsNamedWithItsFieldsAndRemedy(t *testing.T) {
	set := schemacheck.Embedded()
	older := schemachecktest.Without(t, schemachecktest.Kind(t, set, "Tenant"), "spec.apps[].addonPins")
	api := newCluster(t, without(t, set, older)...)
	checker, gate, events := newChecker(api)

	report := checker.CheckOnce(context.Background())
	tenant := kindOf(t, report, "Tenant")
	if report.OK || tenant.State != schemacheck.StateOutdated || !reflect.DeepEqual(tenant.Missing, []string{"spec.apps[].addonPins"}) {
		t.Fatalf("report: ok=%v tenant=%+v", report.OK, tenant)
	}
	if tenant.Remedy != schemacheck.RemedyChart {
		t.Errorf("a chart CRD's remedy is the Argo CD sync, got %q", tenant.Remedy)
	}
	hold := gate.Hold(schemacheck.Chart)
	if hold == nil || hold.Reason != schemacheck.ReasonOutdated {
		t.Fatalf("hold: %+v", hold)
	}
	for _, want := range []string{"Tenant", "spec.apps[].addonPins", "sync the gentian-os Application"} {
		if !strings.Contains(hold.Message, want) {
			t.Errorf("the hold does not name %q: %s", want, hold.Message)
		}
	}
	// Something that writes only kinds Crossplane generates is not held by it.
	if h := gate.Hold(schemacheck.Kinds(set, "App")); h != nil {
		t.Errorf("a writer of App is held by an outdated Tenant: %+v", h)
	}
	select {
	case e := <-events.Events:
		if !strings.Contains(e, schemacheck.ReasonOutdated) || !strings.Contains(e, "spec.apps[].addonPins") {
			t.Errorf("event: %s", e)
		}
	default:
		t.Error("no Event was recorded")
	}

	// Recovery: the definition is updated, and the next check lets go.
	api.replace(t, schemachecktest.Kind(t, set, "Tenant"))
	if report := checker.CheckOnce(context.Background()); !report.OK {
		t.Fatalf("after the update: %+v", report.NotOK())
	}
	if h := gate.Hold(schemacheck.Chart); h != nil {
		t.Fatalf("still held after the update: %+v", h)
	}
}

// An XRD's generated CRDs are what is checked, both of them: a claim that
// kept a field its composite dropped loses it one step later.
func TestAnOutdatedGeneratedDefinitionHoldsTheWritersOfEitherHalf(t *testing.T) {
	set := schemacheck.Embedded()
	for _, half := range []string{"App", "XApp"} {
		older := schemachecktest.Without(t, schemachecktest.Kind(t, set, half), "spec.pullSecrets")
		checker, gate, _ := newChecker(newCluster(t, without(t, set, older)...))
		report := checker.CheckOnce(context.Background())
		got := kindOf(t, report, half)
		if got.State != schemacheck.StateOutdated || !reflect.DeepEqual(got.Missing, []string{"spec.pullSecrets"}) || got.Remedy != schemacheck.RemedyXRD {
			t.Fatalf("%s: %+v", half, got)
		}
		hold := gate.Hold(schemacheck.Kinds(set, "App"))
		if hold == nil || hold.Reason != schemacheck.ReasonOutdated || !strings.Contains(hold.Message, "./install.sh --only B-06") {
			t.Fatalf("%s outdated, writer of App: %+v", half, hold)
		}
		if h := gate.Hold(schemacheck.Chart, schemacheck.Kinds(set, "XTenant")); h != nil {
			t.Errorf("%s outdated holds a writer of XTenant: %+v", half, h)
		}
	}
}

// The fresh install: Crossplane generates a CRD some time after its XRD is
// applied. Not there yet is not "outdated", is not logged as an error, holds
// only what writes that kind, and clears by itself.
func TestADefinitionNotThereYetIsNotReadyAndClearsByItself(t *testing.T) {
	set := schemacheck.Embedded()
	api := newCluster(t, without(t, set, absent("apps.gentianos.io"), absent("xapps.gentianos.io"))...)
	checker, gate, _ := newChecker(api)

	report := checker.CheckOnce(context.Background())
	app := kindOf(t, report, "App")
	if app.State != schemacheck.StateAbsent || len(app.Missing) != 0 {
		t.Fatalf("an absent definition: %+v", app)
	}
	if !strings.Contains(app.Remedy, "Crossplane generates it from xapps.gentianos.io") {
		t.Errorf("remedy: %s", app.Remedy)
	}
	hold := gate.Hold(schemacheck.Kinds(set, "App"))
	if hold == nil || hold.Reason != schemacheck.ReasonNotReady || !strings.Contains(hold.Message, "not on the cluster yet") {
		t.Fatalf("writer of an absent kind: %+v", hold)
	}
	// Nothing that does not write the kind waits for it.
	if h := gate.Hold(schemacheck.Chart, schemacheck.Kinds(set, "XTenant")); h != nil {
		t.Fatalf("an absent App holds something that does not write it: %+v", h)
	}

	api.replace(t, schemachecktest.Kind(t, set, "App"))
	api.replace(t, schemachecktest.Kind(t, set, "XApp"))
	if report := checker.CheckOnce(context.Background()); !report.OK {
		t.Fatalf("after Crossplane generated them: %+v", report.NotOK())
	}
	if h := gate.Hold(schemacheck.Kinds(set, "App")); h != nil {
		t.Fatalf("still held: %+v", h)
	}
}

// A kind no reconciler writes may stay absent for good without holding
// anything: the report says so and that is all.
func TestAKindNobodyWritesMayStayAbsent(t *testing.T) {
	set := schemacheck.Embedded()
	checker, gate, _ := newChecker(newCluster(t, without(t, set, absent("suze.gentianos.io"), absent("xsuze.gentianos.io"))...))
	report := checker.CheckOnce(context.Background())
	if report.OK {
		t.Fatal("an absent definition is not a cluster in order")
	}
	if h := gate.Hold(schemacheck.Chart, schemacheck.Kinds(set, "App", "XTenant")); h != nil {
		t.Fatalf("held by a kind nothing here writes: %+v", h)
	}
}

// The operator's permission to read definitions comes with the chart. Being
// refused it is a chart older than the operator, so it holds and names the
// sync -- it is not waved through as "could not check".
func TestADefinitionTheOperatorMayNotReadHolds(t *testing.T) {
	set := schemacheck.Embedded()
	api := newCluster(t, schemachecktest.Current(set)...)
	for _, name := range set.CRDNames() {
		api.fail[name] = apierrors.NewForbidden(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, name, errors.New("no"))
	}
	checker, gate, _ := newChecker(api)
	report := checker.CheckOnce(context.Background())
	if got := kindOf(t, report, "Tenant"); got.State != schemacheck.StateUnreadable || got.Remedy != schemacheck.RemedyChart {
		t.Fatalf("%+v", got)
	}
	hold := gate.Hold(schemacheck.Chart)
	if hold == nil || hold.Reason != schemacheck.ReasonUnconfirmed || !strings.Contains(hold.Message, "sync the gentian-os Application") {
		t.Fatalf("hold: %+v", hold)
	}

	clear(api.fail)
	if report := checker.CheckOnce(context.Background()); !report.OK {
		t.Fatalf("after the ClusterRole arrived: %+v", report.NotOK())
	}
}

// One slow answer from the API server changes nothing that was established:
// a healthy operator keeps reconciling and a held one stays held.
func TestAReadThatFailsInPassingKeepsWhatWasEstablished(t *testing.T) {
	set := schemacheck.Embedded()
	api := newCluster(t, schemachecktest.Current(set)...)
	checker, gate, _ := newChecker(api)

	// Never read: nothing is established, so the kind is unconfirmed.
	api.fail["tenants.gentianos.io"] = apierrors.NewServiceUnavailable("etcd is slow")
	checker.CheckOnce(context.Background())
	if h := gate.Hold(schemacheck.Chart); h == nil || h.Reason != schemacheck.ReasonUnconfirmed {
		t.Fatalf("a definition never read: %+v", h)
	}

	delete(api.fail, "tenants.gentianos.io")
	checker.CheckOnce(context.Background())
	api.fail["tenants.gentianos.io"] = apierrors.NewServiceUnavailable("etcd is slow")
	report := checker.CheckOnce(context.Background())
	if got := kindOf(t, report, "Tenant"); got.State != schemacheck.StateOK {
		t.Fatalf("a passing failure overwrote what was read: %+v", got)
	}
	if h := gate.Hold(schemacheck.Chart); h != nil {
		t.Fatalf("a healthy operator was held by one failed read: %+v", h)
	}
}

// The operator may read exactly the definitions it checks: no fewer, or the
// check answers "unreadable" for a kind on a correctly installed cluster, and
// no more.
func TestTheClusterRoleNamesExactlyTheDefinitionsChecked(t *testing.T) {
	raw, err := os.ReadFile("../../../charts/gentian-os/templates/clusterrole.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, rules, ok := strings.Cut(string(raw), "\nrules:\n")
	if !ok {
		t.Fatal("the chart's ClusterRole has no rules")
	}
	var role struct {
		Rules []struct {
			APIGroups     []string `json:"apiGroups"`
			Resources     []string `json:"resources"`
			ResourceNames []string `json:"resourceNames"`
			Verbs         []string `json:"verbs"`
		} `json:"rules"`
	}
	if err := yaml.Unmarshal([]byte("rules:\n"+rules), &role); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, rule := range role.Rules {
		if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "apiextensions.k8s.io" {
			continue
		}
		found = true
		if !reflect.DeepEqual(rule.Resources, []string{"customresourcedefinitions"}) || !reflect.DeepEqual(rule.Verbs, []string{"get"}) {
			t.Errorf("the operator's grant on definitions must be get on customresourcedefinitions and nothing else, got %v on %v", rule.Verbs, rule.Resources)
		}
		got := append([]string(nil), rule.ResourceNames...)
		sort.Strings(got)
		if want := schemacheck.Embedded().CRDNames(); !reflect.DeepEqual(got, want) {
			t.Errorf("the ClusterRole names %v\nthe check reads %v\nrun `make manifests`", got, want)
		}
	}
	if !found {
		t.Fatal("the chart's ClusterRole grants no read of customresourcedefinitions: the check would find every definition unreadable")
	}
}
