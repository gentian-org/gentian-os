/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

const clusterModels = "/v1/clusters/" + dt.Cluster + "/models"

const modelsBody = `{"enabled": true, "gpuAcceleration": true,
 "instances": [{"name": "qwen", "modelId": "Qwen/Qwen2.5-7B-Instruct"}],
 "providers": [{"name": "infomaniak", "apiBase": "https://api.infomaniak.com/2/ai/12345/openai/v1",
   "apiKeyProperty": "infomaniak_api_key",
   "models": [{"name": "gemma-4-31b", "model": "google/gemma-4-31B-it", "maxTokens": 8192}]}]}`

// The cluster's administrator reads and writes the model settings, each
// asked as one relation of the cluster; the write is a commit in their name
// that records the decision, and the claim then declares the models.
func TestTheClustersAdministratorSetsTheModels(t *testing.T) {
	h := startSeeded(t, nil, nil, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	cluster := "cluster:" + dt.Cluster

	h.asked.reset()
	code, out := h.do(t, "GET", clusterModels, alice, "")
	if code != http.StatusOK || len(out["models"].([]any)) != 0 || out["settings"].(map[string]any)["enabled"] != false {
		t.Fatalf("before: %d %v", code, out)
	}
	if asked := h.asked.questions(); len(asked) != 1 || asked[0] != "user:alice can_audit "+cluster {
		t.Errorf("the read asked %v", asked)
	}

	h.asked.reset()
	code, out = h.do(t, "PUT", clusterModels, alice, modelsBody)
	if code != http.StatusAccepted || out["commit"] == nil {
		t.Fatalf("set: %d %v", code, out)
	}
	if asked := h.asked.questions(); len(asked) != 1 || asked[0] != "user:alice can_configure "+cluster {
		t.Errorf("the write asked %v", asked)
	}
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%(trailers:key=Gentian-Authz,valueonly)", "main")
	if !strings.Contains(trailer, "Alice|") || !strings.Contains(trailer, "user:alice can_configure "+cluster+" allowed") {
		t.Errorf("commit = %q", trailer)
	}
	claim := dt.RemoteFile(t, h.remote, dt.ClaimPath)
	for _, want := range []string{"modelId: Qwen/Qwen2.5-7B-Instruct", "apiKeyProperty: infomaniak_api_key", "maxTokens: 8192", "enabled: true"} {
		if !strings.Contains(claim, want) {
			t.Errorf("the claim lacks %q:\n%s", want, claim)
		}
	}

	// Read back: the settings, and each model under the gateway's name with
	// what the claim says of it. The provider's names where its token belongs.
	_, out = h.do(t, "GET", clusterModels, alice, "")
	models := out["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %v", models)
	}
	instance, provider := models[0].(map[string]any), models[1].(map[string]any)
	if instance["name"] != "qwen-qwen2.5-7b-instruct" || instance["state"] != "declared" {
		t.Errorf("the instance's model = %v", instance)
	}
	if provider["name"] != "infomaniak/gemma-4-31b" || provider["state"] != "declared" ||
		provider["credential"] != "llm-provider-infomaniak" || provider["apiKeyProperty"] != "infomaniak_api_key" {
		t.Errorf("the provider's model = %v", provider)
	}

	// The same again commits nothing.
	tip := h.tip(t)
	if code, out := h.do(t, "PUT", clusterModels, alice, modelsBody); code != http.StatusOK || out["status"] != "unchanged" || h.tip(t) != tip {
		t.Errorf("again: %d %v", code, out)
	}
}

// Nobody but the cluster's administrator: a tenant's administrator holds no
// relation that reaches the route, to read or to write, and another
// cluster's id is not this director's.
func TestATenantsAdministratorDoesNotReachTheModels(t *testing.T) {
	h := start(t)
	before := h.tip(t)
	for who, token := range map[string]string{
		"a tenant's administrator": h.token(t, "tenant-demo", "tom"),
		"a tenant's member":        h.token(t, "tenant-demo", "mia"),
	} {
		if code, _ := h.do(t, "GET", clusterModels, token, ""); code != http.StatusForbidden {
			t.Errorf("%s reads the models: %d", who, code)
		}
		if code, _ := h.do(t, "PUT", clusterModels, token, modelsBody); code != http.StatusForbidden {
			t.Errorf("%s writes the models: %d", who, code)
		}
	}
	if code, _ := h.do(t, "PUT", clusterModels, "", modelsBody); code != http.StatusUnauthorized {
		t.Errorf("nobody writes the models: %d", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/clusters/elsewhere/models", h.token(t, "gentian", "alice"), modelsBody); code != http.StatusBadRequest {
		t.Errorf("another cluster's models: %d", code)
	}
	if h.tip(t) != before {
		t.Error("a refused request committed")
	}
}

// A body the claim's schema would refuse is answered 422 naming the field,
// and one that carries a token, or any field the settings have none of, is
// not read at all. Neither commits.
func TestModelSettingsAreHeldToTheSchemaBeforeAnythingIsWritten(t *testing.T) {
	h := startSeeded(t, nil, nil, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)

	code, out := h.do(t, "PUT", clusterModels, alice, strings.Replace(modelsBody, "https://api.infomaniak.com", "http://api.infomaniak.com", 1))
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "spec.llm.providers[0].apiBase") {
		t.Errorf("no TLS: %d %v", code, out)
	}
	code, out = h.do(t, "PUT", clusterModels, alice, strings.Replace(modelsBody, `"apiKeyProperty"`, `"apiKey": "sk-secret", "apiKeyProperty"`, 1))
	if code != http.StatusBadRequest || strings.Contains(out["error"].(string), "sk-secret") {
		t.Errorf("a token in the body: %d %v", code, out)
	}
	code, out = h.do(t, "PUT", clusterModels, alice, strings.Replace(modelsBody, `"infomaniak_api_key"`, `"other_api_key"`, 1))
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "apiKeyProperty must be infomaniak_api_key") {
		t.Errorf("another provider's token: %d %v", code, out)
	}
	code, out = h.do(t, "PUT", clusterModels, alice, strings.Replace(modelsBody, "https://api.infomaniak.com/2/ai/12345/openai/v1", "https://keycloak.kernel-authentication.svc/v1", 1))
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "apiBase is refused") {
		t.Errorf("a Service of the cluster: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Error("a refused request committed")
	}
}

// A provider's address is asked what it resolves to when it is new to the
// claim, and one that resolves to something that is not public is refused. An
// address the claim already carries is not asked again, and a director with
// nothing to ask sets no new address.
func TestANewProviderAddressMustResolvePublicly(t *testing.T) {
	var vetted []string
	refuse := ""
	vet := func(cfg *api.Config) {
		withFetcher(nil)(cfg)
		cfg.Catalogue.Vet = func(_ context.Context, address string) error {
			vetted = append(vetted, address)
			if address == refuse {
				return fmt.Errorf("%w: its host resolves to 10.0.0.7: it is in 10.0.0.0/8, which is not a public address", catalogue.ErrAddressRefused)
			}
			return nil
		}
	}
	h := startSeeded(t, nil, nil, vet)
	alice := h.token(t, "gentian", "alice")
	const address = "https://api.infomaniak.com/2/ai/12345/openai/v1"

	refuse = address
	before := h.tip(t)
	code, out := h.do(t, "PUT", clusterModels, alice, modelsBody)
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "spec.llm.providers[0].apiBase is refused: its host resolves to 10.0.0.7") {
		t.Fatalf("an address that resolves inside: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused address was committed")
	}

	refuse, vetted = "", nil
	if code, out := h.do(t, "PUT", clusterModels, alice, modelsBody); code != http.StatusAccepted {
		t.Fatalf("a public address: %d %v", code, out)
	}
	if len(vetted) != 1 || vetted[0] != address {
		t.Fatalf("asked about %v", vetted)
	}
	// Another change with the same address asks nothing.
	vetted = nil
	if code, out := h.do(t, "PUT", clusterModels, alice, strings.Replace(modelsBody, `"maxTokens": 8192`, `"maxTokens": 4096`, 1)); code != http.StatusAccepted {
		t.Fatalf("a change beside the address: %d %v", code, out)
	}
	if len(vetted) != 0 {
		t.Fatalf("an address the claim carries was asked about again: %v", vetted)
	}

	// No checker, no new address.
	bare := start(t)
	if code, _ := bare.do(t, "PUT", clusterModels, bare.token(t, "gentian", "alice"), modelsBody); code != http.StatusServiceUnavailable {
		t.Fatalf("a director that checks no address set one: %d", code)
	}
}

// The console's switch is part of the settings: stated, it is committed.
func TestTheGatewaysConsoleSwitchIsSetWithTheModels(t *testing.T) {
	h := startSeeded(t, nil, nil, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	body := strings.Replace(modelsBody, `"enabled": true,`, `"enabled": true, "console": {"enabled": true},`, 1)
	if code, out := h.do(t, "PUT", clusterModels, alice, body); code != http.StatusAccepted {
		t.Fatalf("set: %d %v", code, out)
	}
	_, out := h.do(t, "GET", clusterModels, alice, "")
	if console, _ := out["settings"].(map[string]any)["console"].(map[string]any); console["enabled"] != true {
		t.Fatalf("read back: %v", out["settings"])
	}
	if claim := dt.RemoteFile(t, h.remote, dt.ClaimPath); !strings.Contains(claim, "    console:\n      enabled: true") {
		t.Fatalf("the claim:\n%s", claim)
	}
}

// Which providers' models a tenant's apps may use, and its budget, are the
// cluster's administrator's: read and written under the cluster, a commit
// of the tenant's manifest. The tenant's own administrator reads them and,
// while not delegated, changes nothing.
func TestTheClustersAdministratorSetsATenantsModelAccess(t *testing.T) {
	h := startSeeded(t, nil, nil, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	tom := h.token(t, "tenant-demo", "tom")
	access := "/v1/clusters/" + dt.Cluster + "/tenants/demo/model-access"
	if code, out := h.do(t, "PUT", clusterModels, alice, modelsBody); code != http.StatusAccepted {
		t.Fatalf("models: %d %v", code, out)
	}

	code, out := h.do(t, "GET", access, alice, "")
	if code != http.StatusOK || len(out["providers"].([]any)) != 0 || out["budget"] != nil {
		t.Fatalf("before: %d %v", code, out)
	}
	allowed := map[string]bool{}
	for _, m := range out["models"].([]any) {
		allowed[m.(map[string]any)["name"].(string)] = m.(map[string]any)["allowed"].(bool)
	}
	if !allowed["qwen-qwen2.5-7b-instruct"] || allowed["infomaniak/gemma-4-31b"] {
		t.Fatalf("before opting in: %v", allowed)
	}

	if code, _ := h.do(t, "PUT", access, tom, `{"providers":["infomaniak"]}`); code != http.StatusForbidden {
		t.Fatalf("the tenant's administrator opted the tenant in: %d", code)
	}
	if code, out := h.do(t, "PUT", access, alice, `{"providers":["elsewhere"]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("a provider the claim does not list: %d %v", code, out)
	}
	code, out = h.do(t, "PUT", access, alice, `{"providers":["infomaniak"],"budget":{"maxBudget":20,"budgetDuration":"1mo"}}`)
	if code != http.StatusAccepted {
		t.Fatalf("set: %d %v", code, out)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	for _, want := range []string{"  llm:\n    providers:\n      - infomaniak\n", "      maxBudget: 20\n", "      budgetDuration: 1mo"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the manifest lacks %q:\n%s", want, manifest)
		}
	}
	_, out = h.do(t, "GET", access, alice, "")
	for _, m := range out["models"].([]any) {
		if m.(map[string]any)["name"] == "infomaniak/gemma-4-31b" && m.(map[string]any)["allowed"] != true {
			t.Errorf("after opting in: %v", m)
		}
	}
	if code, _ := h.do(t, "PUT", access, alice, `{}`); code != http.StatusAccepted {
		t.Fatalf("clear: %d", code)
	}
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); strings.Contains(manifest, "  llm:") {
		t.Errorf("cleared, the manifest still says:\n%s", manifest)
	}
}

// The cluster's administrator delegates the providers to a tenant's own
// administrator with spec.llm.delegated, and sets and clears it, and the
// budget, themselves.
func TestTheClustersAdministratorDelegatesATenantsProviders(t *testing.T) {
	h := startSeeded(t, nil, nil, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	access := "/v1/clusters/" + dt.Cluster + "/tenants/demo/model-access"
	if code, out := h.do(t, "PUT", clusterModels, alice, modelsBody); code != http.StatusAccepted {
		t.Fatalf("models: %d %v", code, out)
	}

	h.asked.reset()
	code, out := h.do(t, "PUT", access, alice, `{"providers":["infomaniak"],"budget":{"maxBudget":20},"delegated":true}`)
	if code != http.StatusAccepted {
		t.Fatalf("delegate: %d %v", code, out)
	}
	if asked := h.asked.questions(); len(asked) != 1 || asked[0] != "user:alice can_configure cluster:"+dt.Cluster {
		t.Errorf("the write asked %v", asked)
	}
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(manifest, "    delegated: true") {
		t.Errorf("the manifest lacks the delegation:\n%s", manifest)
	}
	if _, out := h.do(t, "GET", access, alice, ""); out["delegated"] != true {
		t.Errorf("read back: %v", out)
	}

	// Clearing it keeps the rest.
	if code, out := h.do(t, "PUT", access, alice, `{"providers":["infomaniak"],"budget":{"maxBudget":30}}`); code != http.StatusAccepted {
		t.Fatalf("undelegate: %d %v", code, out)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if strings.Contains(manifest, "delegated") || !strings.Contains(manifest, "      maxBudget: 30") {
		t.Errorf("after clearing the delegation:\n%s", manifest)
	}
}

// A tenant's administrator reads the tenant's model access always, and
// changes nothing of it while it is not delegated.
func TestATenantsAdministratorChangesNoProvidersUndelegated(t *testing.T) {
	h := startSeeded(t, nil, nil, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	tom := h.token(t, "tenant-demo", "tom")
	access := "/v1/clusters/" + dt.Cluster + "/tenants/demo/model-access"
	if code, out := h.do(t, "PUT", clusterModels, alice, modelsBody); code != http.StatusAccepted {
		t.Fatalf("models: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", access, alice, `{"budget":{"maxBudget":20}}`); code != http.StatusAccepted {
		t.Fatalf("budget: %d %v", code, out)
	}

	h.asked.reset()
	code, out := h.do(t, "GET", access, tom, "")
	if code != http.StatusOK || out["delegated"] != false || out["budget"].(map[string]any)["maxBudget"] != float64(20) {
		t.Fatalf("the tenant's administrator reads: %d %v", code, out)
	}
	// The cluster's relation first, as the account and as its realm's role
	// holder, then the tenant's.
	if asked := h.asked.questions(); len(asked) != 3 || asked[0] != "user:tom can_audit cluster:"+dt.Cluster ||
		asked[2] != "user:tom can_administer tenant:demo" {
		t.Errorf("the read asked %v", asked)
	}

	before := h.tip(t)
	for _, body := range []string{
		`{"providers":["infomaniak"],"budget":{"maxBudget":20}}`,
		`{"budget":{"maxBudget":20},"delegated":true}`,
		`{"budget":{"maxBudget":20}}`,
	} {
		if code, out := h.do(t, "PUT", access, tom, body); code != http.StatusForbidden {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	if h.tip(t) != before {
		t.Error("a refused request committed")
	}
}

// Delegated, a tenant's administrator opts the tenant in and out of
// providers, a commit in their name that records their relation; the budget
// and the delegation they send as they are, and changing either is refused.
func TestADelegatedTenantsAdministratorChangesTheProviders(t *testing.T) {
	h := startSeeded(t, nil, nil, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	tom := h.token(t, "tenant-demo", "tom")
	access := "/v1/clusters/" + dt.Cluster + "/tenants/demo/model-access"
	if code, out := h.do(t, "PUT", clusterModels, alice, modelsBody); code != http.StatusAccepted {
		t.Fatalf("models: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", access, alice, `{"budget":{"maxBudget":20,"budgetDuration":"30d"},"delegated":true}`); code != http.StatusAccepted {
		t.Fatalf("delegate: %d %v", code, out)
	}

	code, out := h.do(t, "PUT", access, tom, `{"providers":["infomaniak"],"budget":{"maxBudget":20,"budgetDuration":"30d"},"delegated":true}`)
	if code != http.StatusAccepted {
		t.Fatalf("opt in: %d %v", code, out)
	}
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%(trailers:key=Gentian-Authz,valueonly)", "main")
	if !strings.Contains(trailer, "Tom|") || !strings.Contains(trailer, "user:tom can_administer tenant:demo allowed") {
		t.Errorf("commit = %q", trailer)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	for _, want := range []string{"    providers:\n      - infomaniak\n", "      maxBudget: 20\n", "    delegated: true"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the manifest lacks %q:\n%s", want, manifest)
		}
	}
	if code, out := h.do(t, "PUT", access, tom, `{"providers":["elsewhere"],"budget":{"maxBudget":20,"budgetDuration":"30d"},"delegated":true}`); code != http.StatusUnprocessableEntity {
		t.Errorf("a provider the claim does not list: %d %v", code, out)
	}

	before := h.tip(t)
	for what, body := range map[string]string{
		"a higher budget":   `{"providers":["infomaniak"],"budget":{"maxBudget":200,"budgetDuration":"30d"},"delegated":true}`,
		"another period":    `{"providers":["infomaniak"],"budget":{"maxBudget":20,"budgetDuration":"1d"},"delegated":true}`,
		"no budget":         `{"providers":["infomaniak"],"delegated":true}`,
		"no delegation":     `{"providers":["infomaniak"],"budget":{"maxBudget":20,"budgetDuration":"30d"}}`,
		"nothing at all":    `{}`,
		"a per-minute rate": `{"providers":["infomaniak"],"budget":{"maxBudget":20,"budgetDuration":"30d","rpmLimit":5},"delegated":true}`,
	} {
		if code, out := h.do(t, "PUT", access, tom, body); code != http.StatusForbidden {
			t.Errorf("%s: %d %v", what, code, out)
		}
	}
	if h.tip(t) != before {
		t.Error("a refused request committed")
	}

	// Opting out again is theirs too.
	if code, out := h.do(t, "PUT", access, tom, `{"budget":{"maxBudget":20,"budgetDuration":"30d"},"delegated":true}`); code != http.StatusAccepted {
		t.Fatalf("opt out: %d %v", code, out)
	}
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); strings.Contains(manifest, "infomaniak") {
		t.Errorf("opted out, the manifest still says:\n%s", manifest)
	}
}

// Nobody else: a member of the tenant who does not administer it, and the
// administrator of another tenant, neither read nor write, delegated or not.
func TestOnlyTheTenantsAdministratorReachesItsModelAccess(t *testing.T) {
	h := startSeeded(t, nil, nil, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	access := "/v1/clusters/" + dt.Cluster + "/tenants/demo/model-access"
	if code, out := h.do(t, "PUT", clusterModels, alice, modelsBody); code != http.StatusAccepted {
		t.Fatalf("models: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", access, alice, `{"delegated":true}`); code != http.StatusAccepted {
		t.Fatalf("delegate: %d %v", code, out)
	}
	before := h.tip(t)
	for who, token := range map[string]string{
		"a tenant's member":              h.token(t, "tenant-demo", "mia"),
		"another tenant's administrator": h.token(t, "tenant-user", "uma"),
	} {
		if code, _ := h.do(t, "GET", access, token, ""); code != http.StatusForbidden {
			t.Errorf("%s reads: %d", who, code)
		}
		if code, _ := h.do(t, "PUT", access, token, `{"providers":["infomaniak"],"delegated":true}`); code != http.StatusForbidden {
			t.Errorf("%s writes: %d", who, code)
		}
	}
	if code, _ := h.do(t, "PUT", access, "", `{"delegated":true}`); code != http.StatusUnauthorized {
		t.Errorf("nobody writes: %d", code)
	}
	if h.tip(t) != before {
		t.Error("a refused request committed")
	}
}
