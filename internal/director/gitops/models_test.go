/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// installedLLM is the llm section as the installer writes it on a cluster
// that serves models: the two switches, the two empty lists, and the comments
// that explain them.
const installedLLM = `  llm:
    enabled: true
    gpuAcceleration: false
    # The console of the gateway, off by default.
    # console:
    #   enabled: true
    # The models this cluster serves on its own GPUs.
    instances: []
    #  - name: qwen
    #    modelId: Qwen/Qwen2.5-7B-Instruct
    # External OpenAI-compatible providers.
    providers: []
    #  - name: infomaniak
    #    apiKeyProperty: infomaniak_api_key
  tenantDefaults:
    limitRange:
      defaultCpu: 500m
`

func tokens(n int64) *int64 { return &n }

func someModels() gitops.ModelSettings {
	return gitops.ModelSettings{
		Enabled:         true,
		GPUAcceleration: true,
		Instances: []gitops.ModelInstance{
			{Name: "qwen", ModelID: "Qwen/Qwen2.5-7B-Instruct", MaxModelLen: "8192", GPUMemoryUtilization: "0.85"},
		},
		Providers: []gitops.ModelProvider{
			{
				Name: "infomaniak", DisplayName: "Infomaniak AI Services",
				APIBase: "https://api.infomaniak.com/2/ai/12345/openai/v1", APIKeyProperty: "infomaniak_api_key",
				Models: []gitops.ProviderModel{
					{Name: "gemma-4-31b", Model: "google/gemma-4-31B-it", MaxTokens: tokens(8192)},
					{Name: "bge-m3", Model: "BAAI/bge-m3", Mode: "embedding"},
				},
			},
			{Name: "staged", APIBase: "https://models.example.org/v1", APIKeyProperty: "staged_api_key"},
		},
	}
}

func claimText(t *testing.T, g *gitops.GitOps) string {
	t.Helper()
	file, err := g.TenantFile(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	b, err := os.ReadFile(filepath.Join(root, "kernel", "claims", "cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The fixture claim the gateway's chart is tested with reads as the models
// that test holds the gateway to, under the same names
// (scripts/tests/llm_models.py), and the claim's schema accepts it.
func TestTheFixtureClaimReadsAsTheGatewaysModels(t *testing.T) {
	raw, err := os.ReadFile("../../../crossplane/tests/unit/schema/valid/cluster-llm-models.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var claim struct {
		Spec struct {
			LLM gitops.ModelSettings `json:"llm"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &claim); err != nil {
		t.Fatal(err)
	}
	if err := gitops.ValidateModelSettings(claim.Spec.LLM); err != nil {
		t.Fatalf("the fixture is refused: %v", err)
	}
	var got []string
	for _, m := range claim.Spec.LLM.GatewayModels() {
		got = append(got, m.Name+" "+m.Kind+" "+m.State)
	}
	want := "qwen-qwen2.5-7b-instruct instance not-served, infomaniak/gemma-4-31b provider declared, infomaniak/bge-m3 provider declared"
	if strings.Join(got, ", ") != want {
		t.Fatalf("models = %v\nwant     %s", got, want)
	}
}

// An instance's model is never reported as working: the platform starts no
// instance. Without a GPU, or with model serving off, the gateway does not
// list it at all, and with serving off it lists no provider's model either.
func TestWhatTheClaimSaysAboutAModelsState(t *testing.T) {
	m := someModels()
	states := func(m gitops.ModelSettings) string {
		var out []string
		for _, g := range m.GatewayModels() {
			out = append(out, g.State)
		}
		return strings.Join(out, " ")
	}
	if got := states(m); got != "not-served declared declared" {
		t.Errorf("serving, with a GPU: %s", got)
	}
	m.GPUAcceleration = false
	if got := states(m); got != "not-offered declared declared" {
		t.Errorf("serving, no GPU: %s", got)
	}
	m.Enabled = false
	if got := states(m); got != "not-offered not-offered not-offered" {
		t.Errorf("not serving: %s", got)
	}
	for _, g := range someModels().GatewayModels() {
		if g.Kind == "provider" && (g.Credential != "llm-provider-infomaniak" || g.APIKeyProperty != "infomaniak_api_key") {
			t.Errorf("%s names credential %q property %q", g.Name, g.Credential, g.APIKeyProperty)
		}
		if g.State != gitops.ModelDeclared && g.Reason == "" {
			t.Errorf("%s is %s and does not say why", g.Name, g.State)
		}
	}
}

// One commit makes the claim declare the settings asked for, and the rest of
// the claim -- its comments, its other sections -- stays as it was written.
func TestModelSettingsAreCommittedIntoTheClaimAndReadBack(t *testing.T) {
	g := claimWith(t, installedLLM)
	ctx := context.Background()

	have, err := g.ClusterModels(ctx)
	if err != nil || !have.Enabled || have.GPUAcceleration || len(have.Instances) != 0 || len(have.Providers) != 0 {
		t.Fatalf("before: %+v %v", have, err)
	}
	want := someModels()
	res, err := g.SetClusterModels(ctx, want, tenantMeta())
	if err != nil || !res.Changed || res.Commit == "" {
		t.Fatalf("set: %+v %v", res, err)
	}
	text := claimText(t, g)
	for _, kept := range []string{
		"    # The console of the gateway, off by default.\n    # console:\n    #   enabled: true\n",
		"    # The models this cluster serves on its own GPUs.\n    instances:\n      - name: qwen\n        modelId: Qwen/Qwen2.5-7B-Instruct\n",
		"        gpuMemoryUtilization: \"0.85\"\n        maxModelLen: \"8192\"\n",
		"    #  - name: infomaniak\n",
		"            maxTokens: 8192\n",
		"            mode: embedding\n",
		"        apiKeyProperty: staged_api_key\n        models: []\n",
		"    gpuAcceleration: true\n",
		"  tenantDefaults:\n    limitRange:\n      defaultCpu: 500m\n",
	} {
		if !strings.Contains(text, kept) {
			t.Errorf("the claim lacks %q:\n%s", kept, text)
		}
	}
	got, err := g.ClusterModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Instances) != 1 || len(got.Providers) != 2 || len(got.Providers[0].Models) != 2 ||
		*got.Providers[0].Models[0].MaxTokens != 8192 || got.Instances[0].MaxModelLen != "8192" {
		t.Fatalf("read back: %+v", got)
	}

	// The same again is no commit.
	if res, err := g.SetClusterModels(ctx, want, tenantMeta()); err != nil || res.Changed || res.Status != "unchanged" {
		t.Fatalf("again: %+v %v", res, err)
	}

	// What the settings no longer name is gone from the claim, whoever wrote
	// it there, and a list that did not change is not rewritten.
	fewer := someModels()
	fewer.Providers = fewer.Providers[:1]
	fewer.Providers[0].Models = fewer.Providers[0].Models[1:]
	if res, err := g.SetClusterModels(ctx, fewer, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("fewer: %+v %v", res, err)
	}
	text = claimText(t, g)
	if strings.Contains(text, "gemma") || strings.Contains(text, "staged") || !strings.Contains(text, "bge-m3") ||
		!strings.Contains(text, "modelId: Qwen/Qwen2.5-7B-Instruct") {
		t.Errorf("after removing a model and a provider:\n%s", text)
	}
	if res, err := g.SetClusterModels(ctx, gitops.ModelSettings{}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("none: %+v %v", res, err)
	}
	text = claimText(t, g)
	for _, line := range []string{"    enabled: false\n", "    gpuAcceleration: false\n", "    instances: []\n", "    providers: []\n"} {
		if !strings.Contains(text, line) {
			t.Errorf("an emptied claim lacks %q:\n%s", line, text)
		}
	}
}

// A claim that says nothing of models, or says so in one of the short ways,
// takes the settings all the same.
func TestModelSettingsGoIntoAClaimWithNoLLMSection(t *testing.T) {
	for name, spec := range map[string]string{
		"no section":        "",
		"an empty section":  "  llm: {}\n",
		"commented":         "  # llm:\n  #   enabled: false\n  #   instances: []\n",
		"only the switches": "  llm:\n    enabled: true\n",
	} {
		g := claimWith(t, spec)
		want := someModels()
		if res, err := g.SetClusterModels(context.Background(), want, tenantMeta()); err != nil || !res.Changed {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
		got, err := g.ClusterModels(context.Background())
		if err != nil || !got.Enabled || !got.GPUAcceleration || len(got.Instances) != 1 || len(got.Providers) != 2 {
			t.Fatalf("%s: read back %+v %v\n%s", name, got, err, claimText(t, g))
		}
	}
	// Only a switch, on a claim that has no section to put it under.
	g := claimWith(t, "  llm: {}\n")
	if res, err := g.SetClusterModels(context.Background(), gitops.ModelSettings{Enabled: true}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("a switch alone: %+v %v", res, err)
	}
	if got, err := g.ClusterModels(context.Background()); err != nil || !got.Enabled {
		t.Fatalf("a switch alone reads back %+v %v", got, err)
	}
	// A section written on one line is not edited.
	g = claimWith(t, "  llm: {enabled: true}\n")
	if _, err := g.SetClusterModels(context.Background(), someModels(), tenantMeta()); !errors.Is(err, gitops.ErrModelsFlowForm) {
		t.Fatalf("a one-line section: %v", err)
	}
}

// What the claim's schema would refuse is refused before git, with the
// field named, and so is what would give the gateway one name twice.
func TestModelSettingsTheSchemaWouldRefuseAreRefused(t *testing.T) {
	g := claimWith(t, installedLLM)
	before := claimText(t, g)
	for name, c := range map[string]struct {
		break_ func(*gitops.ModelSettings)
		says   string
	}{
		"an instance name that is no Kubernetes name": {func(m *gitops.ModelSettings) { m.Instances[0].Name = "Qwen_1" }, "spec.llm.instances[0].name"},
		"an instance with no model id":                {func(m *gitops.ModelSettings) { m.Instances[0].ModelID = "" }, "spec.llm.instances[0].modelId is empty"},
		"a provider reached without TLS":              {func(m *gitops.ModelSettings) { m.Providers[0].APIBase = "http://models.example.org/v1" }, "spec.llm.providers[0].apiBase"},
		"a provider name too long for its credential": {func(m *gitops.ModelSettings) {
			m.Providers[0].Name = strings.Repeat("a", 41)
			m.Providers[0].APIKeyProperty = gitops.ProviderKeyProperty(m.Providers[0].Name)
		}, "spec.llm.providers[0].name"},
		"a provider name with a slash":       {func(m *gitops.ModelSettings) { m.Providers[0].Name = "a/b" }, "spec.llm.providers[0].name"},
		"a key property that is no property": {func(m *gitops.ModelSettings) { m.Providers[0].APIKeyProperty = "Key-1" }, "spec.llm.providers[0].apiKeyProperty"},
		"a mode the gateway has none of":     {func(m *gitops.ModelSettings) { m.Providers[0].Models[0].Mode = "vision" }, "spec.llm.providers[0].models[0].mode"},
		"a model name with a space":          {func(m *gitops.ModelSettings) { m.Providers[0].Models[0].Name = "gemma 4" }, "spec.llm.providers[0].models[0].name"},
		"a model id over two lines":          {func(m *gitops.ModelSettings) { m.Providers[0].Models[0].Model = "a\n    apiBase: https://elsewhere" }, "spec.llm.providers[0].models[0].model must be one line"},
		"another provider's token":           {func(m *gitops.ModelSettings) { m.Providers[1].APIKeyProperty = "infomaniak_api_key" }, "spec.llm.providers[1].apiKeyProperty must be staged_api_key"},
		"a property of its own choosing":     {func(m *gitops.ModelSettings) { m.Providers[0].APIKeyProperty = "token" }, "spec.llm.providers[0].apiKeyProperty must be infomaniak_api_key"},
		"a Service of the cluster": {func(m *gitops.ModelSettings) {
			m.Providers[0].APIBase = "https://litellm.system-llm.svc.cluster.local/v1"
		}, "spec.llm.providers[0].apiBase is refused: its host litellm.system-llm.svc.cluster.local is a name inside a cluster"},
		"a single name":                      {func(m *gitops.ModelSettings) { m.Providers[0].APIBase = "https://openbao/v1" }, "apiBase is refused: its host openbao is a single name"},
		"a private address":                  {func(m *gitops.ModelSettings) { m.Providers[0].APIBase = "https://10.0.0.7/v1" }, "apiBase is refused: its host is 10.0.0.7"},
		"the node's metadata":                {func(m *gitops.ModelSettings) { m.Providers[0].APIBase = "https://169.254.169.254/latest" }, "which is not a public address"},
		"another port":                       {func(m *gitops.ModelSettings) { m.Providers[0].APIBase = "https://models.example.org:8200/v1" }, "apiBase is refused: it names port 8200"},
		"credentials in the address":         {func(m *gitops.ModelSettings) { m.Providers[0].APIBase = "https://user:pw@models.example.org/v1" }, "apiBase is refused"},
		"a query in the address":             {func(m *gitops.ModelSettings) { m.Providers[0].APIBase = "https://models.example.org/v1?key=1" }, "apiBase is refused"},
		"no tokens at all":                   {func(m *gitops.ModelSettings) { m.Providers[0].Models[0].MaxTokens = tokens(0) }, "maxTokens must be at least 1"},
		"one provider twice":                 {func(m *gitops.ModelSettings) { m.Providers[1].Name = "infomaniak" }, "repeats the provider name"},
		"one model name twice at a provider": {func(m *gitops.ModelSettings) { m.Providers[0].Models[1].Name = "gemma-4-31b" }, "repeats the gateway model"},
		"two instances of one model": {func(m *gitops.ModelSettings) {
			m.Instances = append(m.Instances, gitops.ModelInstance{Name: "again", ModelID: "qwen/qwen2.5-7b-instruct"})
		}, "repeats the gateway model"},
	} {
		m := someModels()
		c.break_(&m)
		_, err := g.SetClusterModels(context.Background(), m, tenantMeta())
		if !errors.Is(err, gitops.ErrInvalidModels) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want it refused naming %q", name, err, c.says)
		}
	}
	if after := claimText(t, g); after != before {
		t.Errorf("a refusal changed the claim:\n%s", after)
	}
}

// A provider's token is one property of the shared credential, and which one
// follows from the provider's name alone.
func TestAProvidersTokenPropertyFollowsFromItsName(t *testing.T) {
	for name, want := range map[string]string{"infomaniak": "infomaniak_api_key", "acme-eu-1": "acme_eu_1_api_key"} {
		if got := gitops.ProviderKeyProperty(name); got != want {
			t.Errorf("%s reads %s, want %s", name, got, want)
		}
	}
	// A claim somebody edited by hand to read another provider's token is
	// shown for what the gateway makes of it: no model of that provider.
	m := someModels()
	m.Providers[0].APIKeyProperty = "staged_api_key"
	for _, g := range m.GatewayModels() {
		if g.Kind == "provider" && (g.State != gitops.ModelNotOffered || !strings.Contains(g.Reason, "not its own")) {
			t.Errorf("%s = %s (%s)", g.Name, g.State, g.Reason)
		}
	}
}

// The gateway's console switch is read as off where the claim does not state
// it, written when a write states it, and left alone when a write does not.
func TestTheConsoleSwitchIsWrittenOnlyWhenStated(t *testing.T) {
	g := claimWith(t, installedLLM)
	ctx := context.Background()
	have, err := g.ClusterModels(ctx)
	if err != nil || have.Console == nil || have.Console.Enabled {
		t.Fatalf("before: %+v %v", have.Console, err)
	}
	on := someModels()
	on.Console = &gitops.GatewayConsole{Enabled: true}
	if res, err := g.SetClusterModels(ctx, on, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("on: %+v %v", res, err)
	}
	if text := claimText(t, g); !strings.Contains(text, "    console:\n      enabled: true\n") {
		t.Fatalf("the claim does not switch the console on:\n%s", text)
	}
	// Unstated: no change, and no commit.
	if res, err := g.SetClusterModels(ctx, someModels(), tenantMeta()); err != nil || res.Changed {
		t.Fatalf("unstated: %+v %v", res, err)
	}
	if got, _ := g.ClusterModels(ctx); !got.Console.Enabled {
		t.Fatal("a write that did not state the console switched it off")
	}
	off := someModels()
	off.Console = &gitops.GatewayConsole{}
	if res, err := g.SetClusterModels(ctx, off, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("off: %+v %v", res, err)
	}
	if got, _ := g.ClusterModels(ctx); got.Console.Enabled {
		t.Fatal("the console stayed on")
	}
}
