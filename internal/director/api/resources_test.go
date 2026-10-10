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

// Choosing a tenant's plan goes through the director like every other change
// to declared state: a commit by whoever may set one, validated against the
// operator's answer first. Reading the plans, the ceiling or anything else
// the cluster holds is the usher's and is tested there. The operator here is
// a stand-in that answers the way internal/applifecycle does, including the
// plans it marks as not selectable and why.

// operator is the app-lifecycle API as these tests need it: a catalogue of
// plans, a tenant on the second, and the queries it was asked.
type operator struct {
	*httptest.Server
	// selfService records the flag the plans request carried, per tenant.
	selfService map[string]string
	// tenants the operator knows; any other is its 400.
	tenants map[string]bool
	// actor and lastAction record what an action arrived as.
	actor      string
	lastAction string
	lastBody   map[string]any
}

func startOperator(t *testing.T) *operator {
	t.Helper()
	op := &operator{selfService: map[string]string{}, tenants: map[string]bool{"demo": true, "solo": true, "other": true}}
	mux := http.NewServeMux()
	known := func(w http.ResponseWriter, r *http.Request) bool {
		if !op.tenants[r.PathValue("t")] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"tenant \"` + r.PathValue("t") + `\" not found"}`))
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/tenants/{t}/resources/plans", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		op.selfService[r.PathValue("t")] = r.URL.Query().Get("selfService")
		plans := []lifecycle.Plan{
			{Name: "nodes-1", DisplayName: "One node", Tier: 0, Quotas: map[string]string{"requestsCpu": "4", "requestsMemory": "16Gi", "cpu": "8", "memory": "32Gi", "storage": "50Gi"},
				Selectable: false, Blocked: "3 cpu committed does not fit under 2", BlockedBy: "fit"},
			{Name: "nodes-2", DisplayName: "Two nodes", Tier: 1, Quotas: map[string]string{"requestsCpu": "8", "requestsMemory": "32Gi", "cpu": "16", "memory": "64Gi", "storage": "100Gi", "maxPods": "60"}, Current: true, Selectable: true},
			{Name: "nodes-3", DisplayName: "Three nodes", Tier: 2, ProductSku: "GTN-N3", Quotas: map[string]string{"requestsCpu": "12", "requestsMemory": "48Gi", "cpu": "24", "memory": "96Gi", "storage": "150Gi", "maxPods": "90"}, Selectable: true},
			{Name: "nodes-4", DisplayName: "Four nodes", Tier: 3, ProductSku: "GTN-N4", Quotas: map[string]string{"requestsCpu": "16", "requestsMemory": "64Gi", "cpu": "32", "memory": "128Gi", "storage": "200Gi", "maxPods": "120"}, Selectable: true},
		}
		if r.URL.Query().Get("selfService") == "true" {
			plans[3].Selectable = false
			plans[3].Blocked = "this plan is arranged with the platform operator, not self-service"
			plans[3].BlockedBy = "self-service"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tenant": r.PathValue("t"), "plans": plans})
	})
	mux.HandleFunc("POST /v1/tenants/{t}/actions/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		op.actor = r.Header.Get("X-Gentian-Actor")
		op.lastAction = r.PathValue("action")
		op.lastBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&op.lastBody)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"action": r.PathValue("action"), "tenant": r.PathValue("t"),
			"name": "manual-20260924-210000", "status": "started",
		})
	})
	// The real operator refuses a request that presents no token, so this one
	// does too: the director's actor header is a claim and not a proof unless
	// only the director can reach the API to set it.
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+operatorToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"this API is the director's; present its token"}`))
			return
		}
		mux.ServeHTTP(w, r)
	})
	op.Server = httptest.NewServer(guarded)
	t.Cleanup(op.Close)
	return op
}

// operatorToken stands in for the director's ServiceAccount token, which the
// stand-in operator here admits by comparison rather than by asking an API
// server.
const operatorToken = "operator-token-for-the-director"

func startWithOperator(t *testing.T) (*harness, *operator) {
	t.Helper()
	op := startOperator(t)
	return startWith(t, lifecycle.New(op.URL, func() string { return operatorToken })), op
}

// A director with no token reaches nothing, which is what a misconfigured
// deployment must look like rather than an open API.
func TestTheOperatorsAPIRefusesADirectorWithNoToken(t *testing.T) {
	op := startOperator(t)
	h := startWith(t, lifecycle.New(op.URL, func() string { return "" }))
	tom := h.token(t, "tenant-demo", "tom")
	code, _ := h.do(t, http.MethodPut, "/v1/tenants/demo/resources", tom, `{"plan":"nodes-3"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("a director presenting no token chose a plan, or the refusal was not named as the gateway fault it is: %d", code)
	}
}

func TestAPlanIsChosenAsACommitAndTheOperatorLearnsItFromGit(t *testing.T) {
	h, _ := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")

	before := h.tip(t)
	code, body := h.do(t, "PUT", "/v1/tenants/demo/resources", tom, `{"plan":"nodes-3"}`)
	if code != http.StatusAccepted || body["status"] != "updated" || body["previousPlan"] != "nodes-2" {
		t.Fatalf("choose: %d %v", code, body)
	}
	if body["commit"] != h.tip(t) || h.tip(t) == before {
		t.Fatalf("answer names %v, remote moved from %s to %s", body["commit"], before, h.tip(t))
	}

	// Choosing the plan git already records is not a commit, whoever asks:
	// the state asked for already holds, and a commit that only renamed the
	// chooser would be a change to the record and not to the tenant.
	after := h.tip(t)
	for _, who := range []string{tom, h.token(t, "gentian", "alice")} {
		code, body = h.do(t, "PUT", "/v1/tenants/demo/resources", who, `{"plan":"nodes-3"}`)
		if code != http.StatusOK || body["status"] != "unchanged" || h.tip(t) != after {
			t.Fatalf("re-choosing the recorded plan: %d %v", code, body)
		}
	}

	// What landed: the patch with the plan's own quantities and the chooser,
	// listed under the kustomization so it is the last word on the quotas.
	patch := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/resource-plan.yaml")
	for _, want := range []string{
		"gentianos.io/resource-plan: nodes-3",
		`gentianos.io/resource-plan-set-by: "tom@example.com"`,
		`requestsCpu: "12"`, `memory: "96Gi"`, "maxPods: 90",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "maxApps") {
		t.Error("a plan must not touch the app cap")
	}
	kustomization := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/kustomization.yaml")
	if !strings.Contains(kustomization, "- tenant.yaml") || !strings.Contains(kustomization, "- path: resource-plan.yaml") {
		t.Fatalf("kustomization:\n%s", kustomization)
	}
	// The commit is the person's, with the decision that allowed it.
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%(trailers:key=Gentian-Authz,valueonly)", "main")
	if !strings.Contains(trailer, "Tom|") || !strings.Contains(trailer, "user:tom can_set_plan tenant:demo allowed") {
		t.Fatalf("commit = %q", trailer)
	}
}

func TestWhoMayChooseAPlanIsTheGraphsAnswer(t *testing.T) {
	h, _ := startWithOperator(t)
	before := h.tip(t)

	// A member may not choose.
	mia := h.token(t, "tenant-demo", "mia")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", mia, `{"plan":"nodes-4"}`); code != http.StatusForbidden {
		t.Fatalf("member chose a plan: %d", code)
	}
	// Another tenant's administrator holds nothing here.
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", tina, `{"plan":"nodes-4"}`); code != http.StatusForbidden {
		t.Fatalf("a stranger chose a plan: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
}

// Whether the caller chooses for themselves is decided from the cluster
// relation, not asserted by a screen: the tenant's administrator is asked
// the self-service catalogue, the platform's is not.
func TestSelfServiceIsDecidedHereNotByTheScreen(t *testing.T) {
	h, op := startWithOperator(t)

	// The arranged plan is refused to a tenant's administrator as the
	// entitlement it lacks, not written.
	tom := h.token(t, "tenant-demo", "tom")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", tom, `{"plan":"nodes-4"}`); code != http.StatusPaymentRequired || op.selfService["demo"] != "true" {
		t.Fatalf("a tenant admin took the arranged plan: %d, selfService=%q", code, op.selfService["demo"])
	}

	alice := h.token(t, "gentian", "alice")
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/resources", alice, `{"plan":"nodes-4"}`); code != http.StatusAccepted || op.selfService["demo"] != "" {
		t.Fatalf("platform admin choosing the arranged plan: %d %v, selfService=%q", code, body, op.selfService["demo"])
	}
}

func TestAPlanTheTenantDoesNotFitUnderIsAConflictUnlessForcedByTheCluster(t *testing.T) {
	h, _ := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")
	alice := h.token(t, "gentian", "alice")

	if code, body := h.do(t, "PUT", "/v1/tenants/demo/resources", tom, `{"plan":"nodes-1"}`); code != http.StatusConflict {
		t.Fatalf("downgrade that does not fit: %d %v", code, body)
	}
	// A tenant admin cannot force their way past the guard: it protects
	// their own workloads, and forcing is the cluster's decision.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", tom, `{"plan":"nodes-1","force":true}`); code != http.StatusForbidden {
		t.Fatalf("a tenant admin forced a downgrade: %d", code)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/resources", alice, `{"plan":"nodes-1","force":true}`); code != http.StatusAccepted {
		t.Fatalf("forced by the platform: %d %v", code, body)
	}
	patch := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/resource-plan.yaml")
	// A key the plan leaves unset is written as null, so the ceiling is the
	// plan's and not a mixture with what the manifest had.
	if !strings.Contains(patch, "resource-plan: nodes-1") || !strings.Contains(patch, "maxPods: null") {
		t.Fatalf("patch:\n%s", patch)
	}
}

func TestAPlanTheClusterDoesNotHaveIsNotFound(t *testing.T) {
	h, _ := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", tom, `{"plan":"nodes-99"}`); code != http.StatusNotFound {
		t.Fatalf("unknown plan: %d", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", tom, `{}`); code != http.StatusBadRequest {
		t.Fatalf("no plan: %d", code)
	}
}

func TestWithoutAnOperatorThereAreNoResourcesRoutes(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", tom, `{"plan":"nodes-3"}`); code != http.StatusNotFound {
		t.Fatalf("resources without an operator: %d", code)
	}
}

// A policy is declared state and a backup is an action, and the API says
// which: a policy answers with a commit, a backup answers with what was
// started. Neither is reachable by someone who does not hold the relation.
func TestAPolicyIsCommittedAndABackupIsStarted(t *testing.T) {
	h, op := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom") // administers demo

	// Declared state: a commit, and the file a reviewer reads.
	before := h.tip(t)
	code, body := h.do(t, "PUT", "/v1/tenants/demo/backup-policy", tom,
		`{"schedule":"0 3 * * *","retention":{"keepDaily":7},"encryption":{"recipients":["age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"]}}`)
	if code != http.StatusAccepted || body["commit"] != h.tip(t) || h.tip(t) == before {
		t.Fatalf("policy write: %d %v", code, body)
	}
	policy := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/backup-policy.yaml")
	for _, want := range []string{"kind: BackupPolicy", "scope: tenant", "tenant: demo", `schedule: "0 3 * * *"`, "keepDaily: 7", "recipients:"} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy is missing %q:\n%s", want, policy)
		}
	}
	kustomization := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/kustomization.yaml")
	if !strings.Contains(kustomization, "- backup-policy.yaml") {
		t.Fatalf("the policy is not applied:\n%s", kustomization)
	}
	// Writing the same thing again changes nothing.
	if code, body = h.do(t, "PUT", "/v1/tenants/demo/backup-policy", tom,
		`{"schedule":"0 3 * * *","retention":{"keepDaily":7},"encryption":{"recipients":["age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"]}}`); code != http.StatusOK || body["status"] != "unchanged" {
		t.Fatalf("rewriting the same policy: %d %v", code, body)
	}

	// An action: what was started, not what was committed, and the tip has
	// not moved because nothing was written to git.
	after := h.tip(t)
	code, body = h.do(t, "POST", "/v1/tenants/demo/actions/backup", tom, `{}`)
	if code != http.StatusAccepted || body["status"] != "started" || body["action"] != "backup" {
		t.Fatalf("action: %d %v", code, body)
	}
	if _, isCommit := body["commit"]; isCommit || h.tip(t) != after {
		t.Fatalf("an action wrote to git: %v", body)
	}
	// The person is named on the request, because there is no commit to
	// read them off.
	if op.actor != "tom@example.com" || op.lastAction != "backup" {
		t.Fatalf("actor=%q action=%q", op.actor, op.lastAction)
	}

	// Clearing goes back to inheriting, which is the file being gone.
	if code, _ := h.do(t, "DELETE", "/v1/tenants/demo/backup-policy", tom, ""); code != http.StatusAccepted {
		t.Fatalf("clear: %d", code)
	}
	if out := dt.Git(t, "", "--git-dir", h.remote, "ls-tree", "--name-only", "main:clusters/"+dt.Cluster+"/tenants/demo"); strings.Contains(out, "backup-policy.yaml") {
		t.Fatalf("the policy file survived the clear:\n%s", out)
	}
}

func TestWhoMaySetAPolicyAndWhoMayTakeABackup(t *testing.T) {
	h, _ := startWithOperator(t)
	before := h.tip(t)

	// A member may read backups and change nothing.
	mia := h.token(t, "tenant-demo", "mia")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/backup-policy", mia, `{"schedule":"0 4 * * *"}`); code != http.StatusForbidden {
		t.Fatalf("a member set the policy: %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/actions/backup", mia, `{}`); code != http.StatusForbidden {
		t.Fatalf("a member took a backup: %d", code)
	}
	// Another tenant's administrator holds nothing here.
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/actions/backup", tina, `{}`); code != http.StatusForbidden {
		t.Fatalf("a stranger took a backup: %d", code)
	}
	// The cluster's own policy is the cluster's to set.
	if code, _ := h.do(t, "PUT", "/v1/clusters/"+dt.Cluster+"/backup-policy", tina, `{"schedule":"0 4 * * *"}`); code != http.StatusForbidden {
		t.Fatalf("a tenant admin set the cluster's policy: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}

	alice := h.token(t, "gentian", "alice") // platform administrator
	code, body := h.do(t, "PUT", "/v1/clusters/"+dt.Cluster+"/backup-policy", alice,
		`{"schedule":"0 1 * * *","allowTenantOverride":false}`)
	if code != http.StatusAccepted || body["commit"] != h.tip(t) {
		t.Fatalf("cluster policy: %d %v", code, body)
	}
	claim := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/kernel/claims/backup-policy.yaml")
	if !strings.Contains(claim, "scope: cluster") || !strings.Contains(claim, "allowTenantOverride: false") {
		t.Fatalf("cluster policy:\n%s", claim)
	}
}

// The realm policy is declared state: read from git, written as a commit, and
// applied by the composition that owns the realm. No Keycloak credential is
// involved at any point, which is what let the console stop holding one.
func TestTheRealmPolicyIsCommittedAndReadBack(t *testing.T) {
	h, _ := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")

	// Nothing declared is a real answer, with the defaults the composition
	// applies beside it.
	code, body := h.do(t, "GET", "/v1/tenants/demo/security-policy", tom, "")
	if code != http.StatusOK {
		t.Fatalf("read: %d %v", code, body)
	}
	if defaults, _ := body["defaults"].(map[string]any); defaults == nil {
		t.Fatalf("no defaults to render: %v", body)
	}

	before := h.tip(t)
	code, body = h.do(t, "PUT", "/v1/tenants/demo/security-policy", tom,
		`{"password":{"minLength":12,"requireDigits":true,"requireUppercase":true,"historyCount":3},`+
			`"session":{"idleMinutes":30,"maxHours":8},`+
			`"bruteForce":{"enabled":true,"maxLoginFailures":5,"lockoutDurationSeconds":900}}`)
	if code != http.StatusAccepted || body["commit"] != h.tip(t) || h.tip(t) == before {
		t.Fatalf("write: %d %v", code, body)
	}

	// What landed is a patch a reviewer can read, applied after the
	// components the tenant pulls in.
	patch := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/security-policy.yaml")
	for _, want := range []string{"kind: Tenant", "security:", "minLength: 12", "requireDigits: true", "historyCount: 3", "idleMinutes: 30", "maxLoginFailures: 5"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, patch)
		}
	}
	// What was not asked for is absent, not zero: the realm keeps the
	// composition's default rather than a zero written over it.
	if strings.Contains(patch, "requireLowercase") || strings.Contains(patch, "maxAgeDays") {
		t.Errorf("an unstated field was written:\n%s", patch)
	}
	kustomization := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/kustomization.yaml")
	if !strings.Contains(kustomization, "- path: security-policy.yaml") {
		t.Fatalf("the policy is not applied:\n%s", kustomization)
	}

	// And it reads back as what was set.
	_, body = h.do(t, "GET", "/v1/tenants/demo/security-policy", tom, "")
	policy, _ := body["policy"].(map[string]any)
	password, _ := policy["password"].(map[string]any)
	if password["minLength"] != float64(12) || password["requireDigits"] != true {
		t.Fatalf("read back: %v", policy)
	}
}

func TestOnlyWhoeverMaySetPolicyChangesTheRealm(t *testing.T) {
	h, _ := startWithOperator(t)
	before := h.tip(t)

	mia := h.token(t, "tenant-demo", "mia") // a member: may look
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/security-policy", mia, ""); code != http.StatusOK {
		t.Fatalf("a member could not read the policy: %d", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/security-policy", mia, `{"password":{"minLength":4}}`); code != http.StatusForbidden {
		t.Fatalf("a member set the realm policy: %d", code)
	}
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/security-policy", tina, `{"password":{"minLength":4}}`); code != http.StatusForbidden {
		t.Fatalf("a stranger set the realm policy: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
}

// The change history is the audit evidence the platform already had: the
// commits, with the decision that allowed each one. What it must not do is
// present a change pushed by hand as though something had authorised it.
func TestTheChangeHistoryNamesWhatAllowedEachChange(t *testing.T) {
	h, _ := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")

	// A change made through the platform.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/security-policy", tom, `{"password":{"minLength":12}}`); code != http.StatusAccepted {
		t.Fatalf("setting up a change: %d", code)
	}

	code, body := h.do(t, "GET", "/v1/tenants/demo/changes", tom, "")
	if code != http.StatusOK {
		t.Fatalf("read: %d %v", code, body)
	}
	// The answer says what it does not cover, because a screen headed
	// "audit" that showed only this would claim more than it has.
	if covers, _ := body["covers"].(string); !strings.Contains(covers, "Sign-ins") {
		t.Fatalf("the answer does not say what it leaves out: %v", body["covers"])
	}
	changes, _ := body["changes"].([]any)
	if len(changes) == 0 {
		t.Fatal("no changes for a tenant that has just been changed")
	}
	latest := changes[0].(map[string]any)
	if latest["throughPlatform"] != true {
		t.Fatalf("a change the director made is not marked as one: %v", latest)
	}
	if latest["principal"] != "tom" || latest["decision"] != "can_set_policy tenant:demo" {
		t.Fatalf("the authority is not reported: %v", latest)
	}
	if latest["requestId"] == "" || latest["commit"] == "" {
		t.Fatalf("nothing to join this to the decision log: %v", latest)
	}
	files, _ := latest["files"].([]any)
	if len(files) == 0 {
		t.Fatalf("no files on a change: %v", latest)
	}

	// The seed commit was pushed by hand, and says so.
	byHand := false
	for _, c := range changes {
		entry := c.(map[string]any)
		if entry["throughPlatform"] == false && entry["decision"] == nil {
			byHand = true
		}
	}
	if !byHand {
		t.Fatal("a commit with no authorization trailer must be reported as one, not hidden")
	}
}

func TestAChangeHistoryIsReadByAdministratorsAndAuditors(t *testing.T) {
	h, _ := startWithOperator(t)

	// A member may enter the tenant and may not read who did what in it.
	mia := h.token(t, "tenant-demo", "mia")
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/changes", mia, ""); code != http.StatusForbidden {
		t.Fatalf("a member read the change history: %d", code)
	}
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/changes", tina, ""); code != http.StatusForbidden {
		t.Fatalf("a stranger read the change history: %d", code)
	}
	// The cluster's whole history is the auditor's view.
	audrey := h.token(t, "gentian", "audrey")
	code, body := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/changes?limit=5", audrey, "")
	if code != http.StatusOK {
		t.Fatalf("cluster changes: %d %v", code, body)
	}
	if changes, _ := body["changes"].([]any); len(changes) == 0 || len(changes) > 5 {
		t.Fatalf("limit not honoured: %d", len(changes))
	}
	if code, _ := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/changes", tina, ""); code != http.StatusForbidden {
		t.Fatalf("a tenant admin read the cluster's history: %d", code)
	}
}

// A grant is declared state: what one app may consume, committed as an
// object a reviewer reads, under the verb model v1 has for deciding it.
func TestAGrantIsCommittedAndWithdrawn(t *testing.T) {
	h, _ := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")

	before := h.tip(t)
	code, body := h.do(t, "PUT", "/v1/tenants/demo/grants/notes", tom,
		`{"consume":[{"contract":"files","granted":["read"]}],"allowConsumers":[{"app":"tasks","contract":"notes","scope":["read"]}]}`)
	if code != http.StatusAccepted || body["commit"] != h.tip(t) || h.tip(t) == before {
		t.Fatalf("grant: %d %v", code, body)
	}
	grant := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/grant-notes.yaml")
	for _, want := range []string{"kind: AppGrant", "name: notes", "namespace: tenant-demo", "app: notes", "contract: files", "- read", "app: tasks"} {
		if !strings.Contains(grant, want) {
			t.Errorf("grant is missing %q:\n%s", want, grant)
		}
	}
	kustomization := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/kustomization.yaml")
	if !strings.Contains(kustomization, "- grant-notes.yaml") {
		t.Fatalf("the grant is not applied:\n%s", kustomization)
	}

	// Withdrawing takes the file and the listing away, which withdraws
	// everything it permitted.
	if code, _ := h.do(t, "DELETE", "/v1/tenants/demo/grants/notes", tom, ""); code != http.StatusAccepted {
		t.Fatalf("withdraw: %d", code)
	}
	if out := dt.Git(t, "", "--git-dir", h.remote, "ls-tree", "--name-only", "main:clusters/"+dt.Cluster+"/tenants/demo"); strings.Contains(out, "grant-notes.yaml") {
		t.Fatalf("the grant survived being withdrawn:\n%s", out)
	}
}

// The waiver allowlist is the cluster's own security configuration. It is
// read under can_audit, joined with what the catalogue asks of it, and
// changed under can_set_admission -- which model v1 binds to break-glass and
// nobody holds by standing membership. That is the model's decision and the
// right one: letting an app out of the pod-security baseline should cost a
// deliberate elevation, not an ordinary admin session.
func TestTheWaiverAllowlistIsReadWidelyAndChangedOnlyByBreakGlass(t *testing.T) {
	h, _ := startWithOperator(t)
	audrey := h.token(t, "gentian", "audrey") // may audit the cluster
	alice := h.token(t, "gentian", "alice")   // platform administrator

	code, body := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/platform-security", audrey, "")
	if code != http.StatusOK {
		t.Fatalf("read: %d %v", code, body)
	}
	// Declared nothing yet: an empty list, and nothing else. What asks for a
	// waiver is the cluster's to say, and this process does not answer for
	// the cluster.
	if list, ok := body["allowedMacWaivers"].([]any); !ok || len(list) != 0 || len(body) != 2 {
		t.Fatalf("the declared allowlist is not all that was answered: %v", body)
	}
	if list, ok := body["allowedClusterRoles"].([]any); !ok || len(list) != 0 {
		t.Fatalf("the permitted cluster roles are not answered as an empty list: %v", body)
	}

	// A platform administrator is refused, and nothing moves.
	before := h.tip(t)
	if code, _ := h.do(t, "PUT", "/v1/clusters/"+dt.Cluster+"/platform-security", alice,
		`{"allowedMacWaivers":[{"profile":"element","policy":"gentian-require-non-root","scope":"synapse"}]}`); code != http.StatusForbidden {
		t.Fatalf("a platform administrator changed the admission posture without break-glass: %d", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/clusters/"+dt.Cluster+"/platform-security", alice,
		`{"allowedClusterRoles":[{"profile":"dashboard","role":"read-nodes"}]}`); code != http.StatusForbidden {
		t.Fatalf("a platform administrator permitted a cluster role without break-glass: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
	// A tenant administrator is refused too, for the ordinary reason.
	if code, _ := h.do(t, "PUT", "/v1/clusters/"+dt.Cluster+"/platform-security",
		h.token(t, "tenant-solo", "tina"), `{"allowedMacWaivers":[]}`); code != http.StatusForbidden {
		t.Fatalf("a tenant admin changed the cluster's admission posture: %d", code)
	}
}

func TestOnlyTheRightVerbsChangeGrantsAndWaivers(t *testing.T) {
	h, _ := startWithOperator(t)
	before := h.tip(t)

	mia := h.token(t, "tenant-demo", "mia")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/grants/notes", mia, `{"consume":[]}`); code != http.StatusForbidden {
		t.Fatalf("a member set a grant: %d", code)
	}
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/grants/notes", tina, `{"consume":[]}`); code != http.StatusForbidden {
		t.Fatalf("a stranger set a grant: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
}

// The director does not answer for the cluster. Every read of live state it
// once relayed is the usher's now, and none of them is a route here any
// more: the path is unknown, or known only for the write that stayed.
func TestLiveStateIsNotServedHere(t *testing.T) {
	h, _ := startWithOperator(t)
	audrey := h.token(t, "gentian", "audrey") // may audit the cluster and view every tenant
	for _, path := range []string{
		"/v1/tenants/demo/apps/status",
		"/v1/tenants/demo/resources",
		"/v1/tenants/demo/resources/plans",
		"/v1/tenants/demo/resources/usage",
		"/v1/tenants/demo/resources/report",
		"/v1/tenants/demo/backups",
		"/v1/tenants/demo/backups/nightly-1",
		"/v1/tenants/demo/backup-policy",
		"/v1/tenants/demo/backup-schedules",
		"/v1/tenants/demo/integrations",
		"/v1/tenants/demo/notifications",
		"/v1/clusters/" + dt.Cluster + "/resources",
		"/v1/clusters/" + dt.Cluster + "/backup-policy",
		"/v1/clusters/" + dt.Cluster + "/backup-schedules",
		"/v1/clusters/" + dt.Cluster + "/customizations",
	} {
		// 405 where a write of declared state keeps the path -- the plan, a
		// backup policy -- and 404 everywhere else.
		if code, body := h.do(t, "GET", path, audrey, ""); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s is still served here: %d %v", path, code, body)
		}
	}
}
