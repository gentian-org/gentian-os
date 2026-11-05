/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// The cluster's plans are files in git the director writes for whoever may
// configure the cluster. Which tenant is on which plan is the operator's
// answer; the stand-in here gives it from a map, or fails when down is set.

type planOperator struct {
	*httptest.Server
	on   map[string]string // tenant -> plan
	down bool
}

func startPlanOperator(t *testing.T) *planOperator {
	t.Helper()
	op := &planOperator{on: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		if op.down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		tenants := []map[string]string{}
		for tenant, plan := range op.on {
			tenants = append(tenants, map[string]string{"tenant": tenant, "plan": plan})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tenants": tenants, "unavailable": []any{}})
	})
	op.Server = httptest.NewServer(mux)
	t.Cleanup(op.Close)
	return op
}

func startWithPlans(t *testing.T) (*harness, *planOperator) {
	t.Helper()
	op := startPlanOperator(t)
	return startWith(t, lifecycle.New(op.URL, func() string { return operatorToken })), op
}

const plansPath = "/v1/clusters/" + dt.Cluster + "/resource-plans"

const basePlan = `{"displayName":"Base","tier":0,"units":1,"default":true,"productSku":"sku-1",
	"quotas":{"requestsCpu":"4","requestsMemory":"16Gi","cpu":"16","memory":"32Gi","storage":"50Gi"}}`

const twoNodes = `{"displayName":"2 nodes","tier":10,"units":2,"productSku":"sku-2",
	"quotas":{"requestsCpu":"8","requestsMemory":"32Gi","cpu":"32","memory":"64Gi","storage":"100Gi"}}`

func TestAClusterAdministratorWritesAPlanAsACommit(t *testing.T) {
	h, op := startWithPlans(t)
	alice := h.token(t, "gentian", "alice")

	if code, body := h.do(t, http.MethodPut, plansPath+"/base", alice, basePlan); code != http.StatusAccepted {
		t.Fatalf("add base: %d %v", code, body)
	}
	before := h.tip(t)
	code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, twoNodes)
	if code != http.StatusAccepted || body["commit"] != h.tip(t) || h.tip(t) == before {
		t.Fatalf("add nodes-2: %d %v", code, body)
	}

	file := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/resource-plans/nodes-2.yaml")
	for _, want := range []string{"kind: ResourcePlan", "name: nodes-2", "units: 2", "tier: 10", `requestsCpu: "8"`, `productSku: "sku-2"`} {
		if !strings.Contains(file, want) {
			t.Errorf("plan file is missing %q:\n%s", want, file)
		}
	}
	kustomization := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/resource-plans/kustomization.yaml")
	if !strings.Contains(kustomization, "- base.yaml") || !strings.Contains(kustomization, "- nodes-2.yaml") {
		t.Fatalf("kustomization:\n%s", kustomization)
	}
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%(trailers:key=Gentian-Authz,valueonly)", "main")
	if !strings.Contains(trailer, "Alice|") || !strings.Contains(trailer, "can_configure cluster:"+dt.Cluster+" allowed") {
		t.Fatalf("commit = %q", trailer)
	}

	// Writing what git holds is not a commit.
	after := h.tip(t)
	if code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, twoNodes); code != http.StatusOK || h.tip(t) != after {
		t.Fatalf("rewriting the same plan: %d %v", code, body)
	}

	op.on["demo"] = "nodes-2"
	code, body = h.do(t, http.MethodGet, plansPath, alice, "")
	if code != http.StatusOK || body["tenantsKnown"] != true {
		t.Fatalf("list: %d %v", code, body)
	}
	plans := body["plans"].([]any)
	if len(plans) != 2 || plans[1].(map[string]any)["name"] != "nodes-2" {
		t.Fatalf("plans = %v", plans)
	}
	if tenants := plans[1].(map[string]any)["tenants"]; tenants == nil || tenants.([]any)[0] != "demo" {
		t.Fatalf("tenants on nodes-2 = %v", tenants)
	}
}

func TestOnlyWhoConfiguresTheClusterWritesPlans(t *testing.T) {
	h, _ := startWithPlans(t)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	if code, _ := h.do(t, http.MethodPut, plansPath+"/base", tom, basePlan); code != http.StatusForbidden {
		t.Fatalf("a tenant's administrator wrote a plan: %d", code)
	}
	if code, _ := h.do(t, http.MethodGet, plansPath, tom, ""); code != http.StatusForbidden {
		t.Fatalf("a tenant's administrator read the cluster's plans: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
}

func TestTheCatalogKeepsOneDefaultAndATierPerPlan(t *testing.T) {
	h, _ := startWithPlans(t)
	alice := h.token(t, "gentian", "alice")

	if code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, twoNodes); code != http.StatusConflict {
		t.Fatalf("the first plan was not the default and was written: %d %v", code, body)
	}
	if code, _ := h.do(t, http.MethodPut, plansPath+"/base", alice, basePlan); code != http.StatusAccepted {
		t.Fatal("add base")
	}
	if code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, strings.Replace(twoNodes, `"tier":10`, `"tier":0`, 1)); code != http.StatusConflict {
		t.Fatalf("a second plan took tier 0: %d %v", code, body)
	}
	if code, body := h.do(t, http.MethodPut, plansPath+"/base", alice, strings.Replace(basePlan, `"default":true`, `"default":false`, 1)); code != http.StatusConflict {
		t.Fatalf("the default stopped being one with no other: %d %v", code, body)
	}

	// Making another plan the default moves the flag, in one commit.
	before := h.tip(t)
	if code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, strings.Replace(twoNodes, `"tier":10`, `"tier":10,"default":true`, 1)); code != http.StatusAccepted {
		t.Fatalf("new default: %d %v", code, body)
	}
	changed := dt.Git(t, "", "--git-dir", h.remote, "diff", "--name-only", before, "main")
	if !strings.Contains(changed, "base.yaml") || !strings.Contains(changed, "nodes-2.yaml") {
		t.Fatalf("one commit should hold both plans, changed:\n%s", changed)
	}
	if strings.Contains(dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/resource-plans/base.yaml"), "default: true") {
		t.Fatal("two defaults")
	}

	if code, body := h.do(t, http.MethodPut, plansPath+"/odd", alice, `{"displayName":"Odd","tier":5,"quotas":{"cpu":"many"}}`); code != http.StatusBadRequest {
		t.Fatalf("a quota that is no quantity: %d %v", code, body)
	}
	if code, body := h.do(t, http.MethodPut, plansPath+"/odd", alice, `{"displayName":"Odd","tier":5,"quotas":{"gpus":"1"}}`); code != http.StatusBadRequest {
		t.Fatalf("a quota no plan has: %d %v", code, body)
	}
}

func TestAPlansQuotasDoNotChangeUnderTheTenantsOnIt(t *testing.T) {
	h, op := startWithPlans(t)
	alice := h.token(t, "gentian", "alice")
	for name, body := range map[string]string{"base": basePlan} {
		if code, _ := h.do(t, http.MethodPut, plansPath+"/"+name, alice, body); code != http.StatusAccepted {
			t.Fatal("seed")
		}
	}
	if code, _ := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, twoNodes); code != http.StatusAccepted {
		t.Fatal("seed nodes-2")
	}
	op.on["demo"] = "nodes-2"
	bigger := strings.Replace(twoNodes, `"requestsCpu":"8"`, `"requestsCpu":"10"`, 1)

	code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, bigger)
	if code != http.StatusConflict || !strings.Contains(body["error"].(string), "tenant demo") {
		t.Fatalf("quotas changed under a tenant: %d %v", code, body)
	}
	// What a plan is called, its SKU and whether it is offered may change.
	retired := strings.Replace(twoNodes, `"tier":10`, `"tier":10,"retired":true`, 1)
	if code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, retired); code != http.StatusAccepted {
		t.Fatalf("retiring a plan in use: %d %v", code, body)
	}
	// The same quantity written another way is no change.
	if code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, strings.Replace(retired, `"32Gi"`, `"32768Mi"`, 1)); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("an equal quantity: %d %v", code, body)
	}

	// Not knowing who is on it is not nobody.
	op.down = true
	if code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, bigger); code != http.StatusServiceUnavailable {
		t.Fatalf("quotas changed blind: %d %v", code, body)
	}
	op.down = false
	delete(op.on, "demo")
	if code, body := h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, bigger); code != http.StatusAccepted {
		t.Fatalf("quotas of a plan nobody is on: %d %v", code, body)
	}
}

func TestAPlanIsRemovedOnlyWhenNobodyIsOnItAndItIsNotTheDefault(t *testing.T) {
	h, op := startWithPlans(t)
	alice := h.token(t, "gentian", "alice")
	_, _ = h.do(t, http.MethodPut, plansPath+"/base", alice, basePlan)
	_, _ = h.do(t, http.MethodPut, plansPath+"/nodes-2", alice, twoNodes)

	if code, _ := h.do(t, http.MethodDelete, plansPath+"/base", alice, ""); code != http.StatusConflict {
		t.Fatalf("the default was removed: %d", code)
	}
	op.on["demo"] = "nodes-2"
	if code, _ := h.do(t, http.MethodDelete, plansPath+"/nodes-2", alice, ""); code != http.StatusConflict {
		t.Fatalf("a plan in use was removed: %d", code)
	}
	delete(op.on, "demo")
	if code, body := h.do(t, http.MethodDelete, plansPath+"/nodes-2", alice, ""); code != http.StatusAccepted {
		t.Fatalf("remove: %d %v", code, body)
	}
	kustomization := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/resource-plans/kustomization.yaml")
	if strings.Contains(kustomization, "nodes-2") {
		t.Fatalf("still listed:\n%s", kustomization)
	}
	if code, _ := h.do(t, http.MethodDelete, plansPath+"/nodes-2", alice, ""); code != http.StatusNotFound {
		t.Fatalf("removing a plan that is gone: %d", code)
	}
}

const orderingPath = "/v1/clusters/" + dt.Cluster + "/plan-ordering"

// The notice a tenant's screen shows in place of a choice is the cluster
// administrator's to write, one action at most, and is cleared by removing
// the file.
func TestTheOrderingNoticeIsTheClusterAdministratorsCommit(t *testing.T) {
	h, _ := startWithPlans(t)
	alice := h.token(t, "gentian", "alice")
	tom := h.token(t, "tenant-demo", "tom")

	if code, body := h.do(t, http.MethodGet, orderingPath, alice, ""); code != http.StatusOK || body["set"] != false {
		t.Fatalf("no notice yet: %d %v", code, body)
	}
	notice := `{"text":{"en":"Change your nodes in the Center","de":"Knoten im Center ändern"},"url":"https://center.example/tenants/{tenant}"}`
	if code, _ := h.do(t, http.MethodPut, orderingPath, tom, notice); code != http.StatusForbidden {
		t.Fatalf("a tenant's administrator wrote the notice: %d", code)
	}
	if code, body := h.do(t, http.MethodPut, orderingPath, alice, notice); code != http.StatusAccepted {
		t.Fatalf("set: %d %v", code, body)
	}
	file := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/kernel/claims/plan-ordering.yaml")
	for _, want := range []string{"kind: PlanOrdering", "name: default", "https://center.example/tenants/{tenant}"} {
		if !strings.Contains(file, want) {
			t.Errorf("notice is missing %q:\n%s", want, file)
		}
	}
	if code, body := h.do(t, http.MethodGet, orderingPath, alice, ""); code != http.StatusOK || body["set"] != true {
		t.Fatalf("read back: %d %v", code, body)
	}

	for _, bad := range []string{
		`{"text":{}}`,
		`{"text":{"english":"x"}}`,
		`{"text":{"en":"x"},"url":"http://plain.example"}`,
		`{"text":{"en":"x"},"url":"https://a.example","email":"sales@a.example"}`,
		`{"text":{"en":"x"},"phone":"044 000"}`,
	} {
		if code, body := h.do(t, http.MethodPut, orderingPath, alice, bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", bad, code, body)
		}
	}

	if code, body := h.do(t, http.MethodDelete, orderingPath, alice, ""); code != http.StatusAccepted {
		t.Fatalf("clear: %d %v", code, body)
	}
	if out := dt.Git(t, "", "--git-dir", h.remote, "ls-tree", "--name-only", "main:clusters/"+dt.Cluster+"/kernel/claims"); strings.Contains(out, "plan-ordering.yaml") {
		t.Fatalf("the notice survived the clear:\n%s", out)
	}
}
