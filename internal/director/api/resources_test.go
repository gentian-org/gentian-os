/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

// A tenant's resources go through the director like everything else: the
// reads are the operator's answers relayed to whoever may view the tenant,
// and choosing a plan is a commit by whoever may set one. The operator here
// is a stand-in that answers the way internal/applifecycle does, including
// the plans it marks as not selectable and why.

// operator is the app-lifecycle API as these tests need it: a catalogue of
// three plans, a tenant on the middle one, and the queries it was asked.
type operator struct {
	*httptest.Server
	// selfService records the flag the plans request carried, per tenant.
	selfService map[string]string
	// tenants the operator knows; any other is its 400.
	tenants map[string]bool
	// actor and lastAction record what an action arrived as.
	actor      string
	lastAction string
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
	mux.HandleFunc("GET /v1/tenants/{t}/resources", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenant": r.PathValue("t"), "plan": "nodes-2", "annotatedPlan": "nodes-2",
			"quota":    []map[string]any{{"resource": "requests.cpu", "used": "3", "hard": "8", "usedRatio": 0.375}},
			"hasQuota": true, "installedApps": 1,
		})
	})
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
	mux.HandleFunc("GET /v1/tenants/{t}/resources/usage", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tenant": r.PathValue("t"), "from": r.URL.Query().Get("from"), "stepSeconds": r.URL.Query().Get("stepSeconds"), "samples": []any{}})
	})
	mux.HandleFunc("GET /v1/tenants/{t}/resources/report", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tenant": r.PathValue("t"), "intervals": []any{}})
	})
	mux.HandleFunc("GET /v1/tenants/{t}/backups", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tenant": r.PathValue("t"), "backups": []any{
			map[string]any{"name": "nightly-1", "phase": "Ready", "platformReadable": true},
		}})
	})
	mux.HandleFunc("GET /v1/tenants/{t}/backup-policy", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope": "tenant", "tenant": r.PathValue("t"), "configured": false,
			"effectiveSchedule": "0 2 * * *", "effectiveBucket": "gentian-backups",
		})
	})
	mux.HandleFunc("GET /v1/tenants/{t}/backup-schedules", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tenant": r.PathValue("t"), "schedules": []any{
			map[string]any{"name": "policy", "managed": true, "schedule": "0 2 * * *"},
		}})
	})
	mux.HandleFunc("GET /v1/backup-policy", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"scope": "cluster", "configured": true, "effectiveSchedule": "0 2 * * *"})
	})
	mux.HandleFunc("GET /v1/backup-schedules", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"schedules": []any{}})
	})
	mux.HandleFunc("POST /v1/tenants/{t}/actions/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !known(w, r) {
			return
		}
		op.actor = r.Header.Get("X-Gentian-Actor")
		op.lastAction = r.PathValue("action")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"action": r.PathValue("action"), "tenant": r.PathValue("t"),
			"name": "manual-20260924-210000", "status": "started",
		})
	})
	op.Server = httptest.NewServer(mux)
	t.Cleanup(op.Close)
	return op
}

func startWithOperator(t *testing.T) (*harness, *operator) {
	t.Helper()
	op := startOperator(t)
	return startWith(t, false, projectedTiles(t), lifecycle.New(op.URL)), op
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

	// A member may look and not choose.
	mia := h.token(t, "tenant-demo", "mia")
	if code, body := h.do(t, "GET", "/v1/tenants/demo/resources", mia, ""); code != http.StatusOK || body["plan"] != "nodes-2" {
		t.Fatalf("member read: %d %v", code, body)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", mia, `{"plan":"nodes-4"}`); code != http.StatusForbidden {
		t.Fatalf("member chose a plan: %d", code)
	}
	// Another tenant's administrator holds nothing here.
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/resources/plans", tina, ""); code != http.StatusForbidden {
		t.Fatalf("a stranger read the plans: %d", code)
	}
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

	tom := h.token(t, "tenant-demo", "tom")
	code, body := h.do(t, "GET", "/v1/tenants/demo/resources/plans", tom, "")
	if code != http.StatusOK || op.selfService["demo"] != "true" {
		t.Fatalf("tenant admin: %d, selfService=%q", code, op.selfService["demo"])
	}
	plans := body["plans"].([]any)
	if top := plans[3].(map[string]any); top["selectable"] != false {
		t.Fatalf("the arranged plan was offered to a tenant admin: %v", top)
	}
	// And choosing it is refused as the entitlement it lacks, not written.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/resources", tom, `{"plan":"nodes-4"}`); code != http.StatusPaymentRequired {
		t.Fatalf("a tenant admin took the arranged plan: %d", code)
	}

	alice := h.token(t, "gentian", "alice")
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/resources/plans", alice, ""); code != http.StatusOK || op.selfService["demo"] != "" {
		t.Fatalf("platform admin: %d, selfService=%q", code, op.selfService["demo"])
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/resources", alice, `{"plan":"nodes-4"}`); code != http.StatusAccepted {
		t.Fatalf("platform admin choosing the arranged plan: %d %v", code, body)
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

func TestTheOperatorsAnswersAreRelayedWithTheirQueries(t *testing.T) {
	h, _ := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")
	code, body := h.do(t, "GET", "/v1/tenants/demo/resources/usage?from=2026-09-01T00:00:00Z&stepSeconds=3600&rm=-rf", tom, "")
	if code != http.StatusOK || body["from"] != "2026-09-01T00:00:00Z" || body["stepSeconds"] != "3600" {
		t.Fatalf("usage: %d %v", code, body)
	}
	if code, body := h.do(t, "GET", "/v1/tenants/demo/resources/report", tom, ""); code != http.StatusOK || body["tenant"] != "demo" {
		t.Fatalf("report: %d %v", code, body)
	}
}

// The cluster's view is every tenant git lists, each answered by the
// operator, and a tenant the operator cannot answer for is named rather than
// dropped.
func TestTheClustersViewNamesEveryTenant(t *testing.T) {
	h, op := startWithOperator(t)
	delete(op.tenants, "other")

	audrey := h.token(t, "gentian", "audrey") // may audit, may not configure
	code, body := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/resources", audrey, "")
	if code != http.StatusOK {
		t.Fatalf("cluster view: %d %v", code, body)
	}
	states := body["tenants"].([]any)
	if len(states) != 2 {
		t.Fatalf("states = %v", states)
	}
	unavailable := body["unavailable"].([]any)
	if len(unavailable) != 1 || unavailable[0].(map[string]any)["tenant"] != "other" {
		t.Fatalf("unavailable = %v", unavailable)
	}

	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/resources", tina, ""); code != http.StatusForbidden {
		t.Fatalf("a tenant admin saw the cluster's view: %d", code)
	}
}

func TestWithoutAnOperatorThereAreNoResourcesRoutes(t *testing.T) {
	h := start(t, false)
	tom := h.token(t, "tenant-demo", "tom")
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/resources", tom, ""); code != http.StatusNotFound {
		t.Fatalf("resources without an operator: %d", code)
	}
}

// Backups are read by whoever may view the tenant: seeing whether a tenant's
// data is being kept is not the same permission as changing how. The
// cluster's own policy is can_audit, like the rest of the cluster's state.
func TestBackupsAreReadByWhoeverMayViewTheTenant(t *testing.T) {
	h, _ := startWithOperator(t)

	mia := h.token(t, "tenant-demo", "mia") // a member: can_view, nothing more
	code, body := h.do(t, "GET", "/v1/tenants/demo/backups", mia, "")
	if code != http.StatusOK {
		t.Fatalf("member reading backups: %d %v", code, body)
	}
	if list, _ := body["backups"].([]any); len(list) != 1 {
		t.Fatalf("backups = %v", body["backups"])
	}
	// The policy says what applies even where the tenant states nothing of
	// its own, which is what the screen renders instead of an empty form.
	code, body = h.do(t, "GET", "/v1/tenants/demo/backup-policy", mia, "")
	if code != http.StatusOK || body["configured"] != false || body["effectiveSchedule"] != "0 2 * * *" {
		t.Fatalf("policy: %d %v", code, body)
	}
	if code, body = h.do(t, "GET", "/v1/tenants/demo/backup-schedules", mia, ""); code != http.StatusOK {
		t.Fatalf("schedules: %d %v", code, body)
	}

	// Another tenant's administrator holds nothing here.
	tina := h.token(t, "tenant-solo", "tina")
	for _, path := range []string{"/v1/tenants/demo/backups", "/v1/tenants/demo/backup-policy", "/v1/tenants/demo/backup-schedules"} {
		if code, _ := h.do(t, "GET", path, tina, ""); code != http.StatusForbidden {
			t.Fatalf("a stranger read %s: %d", path, code)
		}
	}

	// The cluster's own policy: can_audit, which a tenant admin does not hold.
	audrey := h.token(t, "gentian", "audrey")
	if code, body := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/backup-policy", audrey, ""); code != http.StatusOK || body["scope"] != "cluster" {
		t.Fatalf("cluster policy: %d %v", code, body)
	}
	if code, _ := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/backup-policy", tina, ""); code != http.StatusForbidden {
		t.Fatalf("a tenant admin read the cluster's backup policy: %d", code)
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
