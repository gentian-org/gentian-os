/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"net/http"
	"strings"
	"testing"

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
	h := start(t)
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
	// what the claim says of it. The instance's is flagged, for nothing
	// starts an instance; the provider's names where its token belongs.
	_, out = h.do(t, "GET", clusterModels, alice, "")
	models := out["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %v", models)
	}
	instance, provider := models[0].(map[string]any), models[1].(map[string]any)
	if instance["name"] != "qwen-qwen2.5-7b-instruct" || instance["state"] != "not-served" || instance["reason"] == nil {
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
	h := start(t)
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
	if code, _ := h.do(t, "PUT", clusterModels, alice, `{"enabled": true, "console": {"enabled": true}}`); code != http.StatusBadRequest {
		t.Errorf("the gateway's console switched on by this route: %d", code)
	}
	if h.tip(t) != before {
		t.Error("a refused request committed")
	}
}
