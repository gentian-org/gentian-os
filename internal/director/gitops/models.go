/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// The models a cluster's gateway offers are the Cluster claim's
// (spec.llm.instances, spec.llm.providers and the two switches beside them),
// and this is how they are read and changed.
//
// The two lists are written as whole blocks and the two switches as lines,
// with the rest of the claim untouched byte for byte -- the way the
// catalogue's sources are (sources.go). A comment written inside one of the
// two lists is not kept when that list changes; one above or below it is.
//
// No provider's token is here, or passes through here. A provider names the
// property of its credential that holds the token (apiKeyProperty); the token
// is entered at the custodian, under llm-provider-<name>.

// ErrInvalidModels is model settings the claim's schema would refuse, or that
// would give the gateway two models of one name.
var ErrInvalidModels = errors.New("invalid model settings")

// ErrModelsFlowForm is an llm section written on one line, which cannot be
// edited in place.
var ErrModelsFlowForm = errors.New("the llm section of the claim is written on one line; write it as a block to change it here")

// ModelInstance is one entry of spec.llm.instances: a model the cluster
// serves from its own weights.
type ModelInstance struct {
	Name                 string `json:"name"`
	ModelID              string `json:"modelId"`
	GPUMemoryUtilization string `json:"gpuMemoryUtilization,omitempty"`
	MaxModelLen          string `json:"maxModelLen,omitempty"`
	ModelCacheSize       string `json:"modelCacheSize,omitempty"`
	ImageTag             string `json:"imageTag,omitempty"`
	ToolCallParser       string `json:"toolCallParser,omitempty"`
}

// ProviderModel is one model of an external provider.
type ProviderModel struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	MaxTokens *int64 `json:"maxTokens,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// ModelProvider is one entry of spec.llm.providers: an external,
// OpenAI-compatible endpoint and the models of it the gateway offers.
type ModelProvider struct {
	Name           string          `json:"name"`
	DisplayName    string          `json:"displayName,omitempty"`
	APIBase        string          `json:"apiBase"`
	APIKeyProperty string          `json:"apiKeyProperty"`
	Models         []ProviderModel `json:"models"`
}

// ModelSettings is the part of spec.llm that decides which models the gateway
// offers.
type ModelSettings struct {
	Enabled         bool            `json:"enabled"`
	GPUAcceleration bool            `json:"gpuAcceleration"`
	Instances       []ModelInstance `json:"instances"`
	Providers       []ModelProvider `json:"providers"`
}

// The states a declared model is in, as far as the claim alone says.
const (
	// ModelNotOffered: the gateway does not list it -- the cluster serves no
	// models, or it is an instance on a cluster that declares no GPU.
	ModelNotOffered = "not-offered"
	// ModelNotServed: the gateway lists it and nothing answers behind it.
	ModelNotServed = "not-served"
	// ModelDeclared: the gateway lists it and calls its provider; whether
	// the provider's token is there is not something the claim says.
	ModelDeclared = "declared"
)

// GatewayModel is one model as the gateway names it, with what the claim
// alone says about whether it works.
type GatewayModel struct {
	// Name is the name an app asks for.
	Name string `json:"name"`
	// Kind is "instance" or "provider".
	Kind string `json:"kind"`
	// Source is the instance's or the provider's name.
	Source string `json:"source"`
	// Credential and APIKeyProperty name where a provider's token is kept:
	// the credential it is entered under, and the property read from it.
	Credential     string `json:"credential,omitempty"`
	APIKeyProperty string `json:"apiKeyProperty,omitempty"`
	State          string `json:"state"`
	// Reason says why, for every state but declared.
	Reason string `json:"reason,omitempty"`
}

// ProviderCredential is the name of the credential a provider's token is
// entered under.
func ProviderCredential(provider string) string { return "llm-provider-" + provider }

// instanceModelName is the name the gateway's chart gives an instance's model
// (kernel/services/llm/manifests/templates/gateway-config.yaml).
func instanceModelName(modelID string) string {
	return strings.ReplaceAll(strings.ToLower(modelID), "/", "-")
}

// GatewayModels lists every model the settings declare, under the name the
// gateway gives it, instances first, in the claim's order.
//
// The state is read off the claim and off one fact about the platform: it
// starts no vLLM instance, so a model of spec.llm.instances has no server
// behind it unless somebody ran one by hand, and is reported as not served.
// Nothing here asks the gateway or the cluster.
func (m ModelSettings) GatewayModels() []GatewayModel {
	out := []GatewayModel{}
	off := "This cluster serves no models: llm.enabled is false."
	for _, i := range m.Instances {
		g := GatewayModel{
			Name: instanceModelName(i.ModelID), Kind: "instance", Source: i.Name, State: ModelNotServed,
			Reason: "The platform does not start the vLLM instance behind this model. " +
				"It is listed by the gateway and a call to it fails until somebody runs vllm-" + i.Name + "-inference in the gateway's namespace.",
		}
		switch {
		case !m.Enabled:
			g.State, g.Reason = ModelNotOffered, off
		case !m.GPUAcceleration:
			g.State, g.Reason = ModelNotOffered, "Instances are read only when llm.gpuAcceleration is true."
		}
		out = append(out, g)
	}
	for _, p := range m.Providers {
		for _, pm := range p.Models {
			g := GatewayModel{
				Name: p.Name + "/" + pm.Name, Kind: "provider", Source: p.Name,
				Credential: ProviderCredential(p.Name), APIKeyProperty: p.APIKeyProperty, State: ModelDeclared,
			}
			if !m.Enabled {
				g.State, g.Reason = ModelNotOffered, off
			}
			out = append(out, g)
		}
	}
	return out
}

// normalised is the settings with no nil list, so that "none" is written and
// compared as an empty list wherever it came from.
func (m ModelSettings) normalised() ModelSettings {
	out := m
	out.Instances = append([]ModelInstance{}, m.Instances...)
	out.Providers = make([]ModelProvider, len(m.Providers))
	for i, p := range m.Providers {
		p.Models = append([]ProviderModel{}, p.Models...)
		out.Providers[i] = p
	}
	return out
}

// llmSchema is the validator of spec.llm, built from the Cluster definition
// this binary carries: the same schema the cluster holds the claim to.
var llmSchema = sync.OnceValues(func() (validation.SchemaValidator, error) {
	def, ok := schemacheck.Embedded().Kind("Cluster")
	if !ok {
		return nil, errors.New("no Cluster definition is built in")
	}
	names := make([]string, 0, len(def.Versions))
	for name := range def.Versions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		llm, ok := def.Versions[name].Properties["spec"].Properties["llm"]
		if !ok {
			continue
		}
		var internal apiextensions.JSONSchemaProps
		if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&llm, &internal, nil); err != nil {
			return nil, err
		}
		sv, _, err := validation.NewSchemaValidator(&internal)
		return sv, err
	}
	return nil, errors.New("the Cluster definition has no spec.llm")
})

// ValidateModelSettings holds the settings to the claim's schema, and to what
// the schema cannot say: a name or an address is one line of printable text,
// a model id is not empty, and no two entries give the gateway one name.
func ValidateModelSettings(m ModelSettings) error {
	m = m.normalised()
	var problems []string
	sv, err := llmSchema()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	for _, e := range validation.ValidateCustomResource(field.NewPath("spec", "llm"), obj, sv) {
		problems = append(problems, e.Error())
	}

	line := func(path, v string, required bool) {
		if required && strings.TrimSpace(v) == "" {
			problems = append(problems, path+" is empty")
		}
		if v != strings.TrimSpace(v) || strings.ContainsFunc(v, func(r rune) bool { return unicode.IsControl(r) || !unicode.IsPrint(r) }) {
			problems = append(problems, path+" must be one line of printable text with no space at either end")
		}
	}
	seen := map[string]string{}
	once := func(kind, name, path string) {
		key := kind + "\x00" + name
		if first, dup := seen[key]; dup {
			problems = append(problems, fmt.Sprintf("%s repeats the %s %q of %s", path, kind, name, first))
			return
		}
		seen[key] = path
	}
	for i, in := range m.Instances {
		at := fmt.Sprintf("spec.llm.instances[%d]", i)
		once("instance name", in.Name, at+".name")
		line(at+".modelId", in.ModelID, true)
		if strings.ContainsAny(in.ModelID, " \t") {
			problems = append(problems, at+".modelId must not contain a space")
		}
		once("gateway model", instanceModelName(in.ModelID), at+".modelId")
		line(at+".gpuMemoryUtilization", in.GPUMemoryUtilization, false)
		line(at+".maxModelLen", in.MaxModelLen, false)
		line(at+".modelCacheSize", in.ModelCacheSize, false)
		line(at+".imageTag", in.ImageTag, false)
		line(at+".toolCallParser", in.ToolCallParser, false)
	}
	for i, p := range m.Providers {
		at := fmt.Sprintf("spec.llm.providers[%d]", i)
		once("provider name", p.Name, at+".name")
		line(at+".displayName", p.DisplayName, false)
		line(at+".apiBase", p.APIBase, true)
		if strings.ContainsAny(p.APIBase, " \t") {
			problems = append(problems, at+".apiBase must not contain a space")
		}
		for j, pm := range p.Models {
			mat := fmt.Sprintf("%s.models[%d]", at, j)
			line(mat+".model", pm.Model, true)
			once("gateway model", p.Name+"/"+pm.Name, mat+".name")
			if pm.MaxTokens != nil && *pm.MaxTokens < 1 {
				problems = append(problems, mat+".maxTokens must be at least 1")
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidModels, strings.Join(problems, "; "))
	}
	return nil
}

// claimModels reads the model settings from the claim's text.
func claimModels(text string) (ModelSettings, error) {
	var claim struct {
		Spec struct {
			LLM ModelSettings `json:"llm"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &claim); err != nil {
		return ModelSettings{}, fmt.Errorf("parse cluster claim: %w", err)
	}
	return claim.Spec.LLM.normalised(), nil
}

// ClusterModels reads the model settings the Cluster claim declares. A claim
// with no llm section declares none, and serves no models.
func (g *GitOps) ClusterModels(ctx context.Context) (ModelSettings, error) {
	var claim struct {
		Spec struct {
			LLM ModelSettings `json:"llm"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		return ModelSettings{}, err
	}
	return claim.Spec.LLM.normalised(), nil
}

// SetClusterModels makes the claim declare exactly these model settings, in
// one commit. What the claim's schema would refuse is refused here, before
// anything is written. A model that is in the claim and not in want is
// removed, whoever added it.
func (g *GitOps) SetClusterModels(ctx context.Context, want ModelSettings, meta Meta) (Result, error) {
	want = want.normalised()
	if err := ValidateModelSettings(want); err != nil {
		return Result{}, err
	}
	msg := fmt.Sprintf("feat(cluster): set the models the gateway offers (via %s)", meta.actor())
	return g.applyClaim(ctx, msg, meta, func(text string) (string, string, bool, error) {
		have, err := claimModels(text)
		if err != nil {
			return text, "", false, err
		}
		if reflect.DeepEqual(have, want) {
			return text, "unchanged", false, nil
		}
		out := text
		// The lists first: writing one makes `llm:` a block where the claim
		// has none or says `llm: {}`, which the two switches then go under.
		// A list that does not change is left as it is written.
		if !reflect.DeepEqual(have.Instances, want.Instances) || !llmBlock.MatchString(text) {
			if out, err = setSpecBlock(out, []string{"llm", "instances"}, renderInstances(want.Instances)); err != nil {
				return text, "", false, modelsEditError(err)
			}
		}
		if !reflect.DeepEqual(have.Providers, want.Providers) {
			if out, err = setSpecBlock(out, []string{"llm", "providers"}, renderProviders(want.Providers)); err != nil {
				return text, "", false, modelsEditError(err)
			}
		}
		for _, sw := range []struct {
			path      string
			have, set bool
		}{
			{"llm.enabled", have.Enabled, want.Enabled},
			{"llm.gpuAcceleration", have.GPUAcceleration, want.GPUAcceleration},
		} {
			if sw.have == sw.set {
				continue
			}
			if out, _, err = setClaimValue(out, sw.path, strconv.FormatBool(sw.set)); err != nil {
				return text, "", false, err
			}
		}
		back, err := claimModels(out)
		if err != nil {
			return text, "", false, fmt.Errorf("the edited claim is not valid YAML: %w", err)
		}
		if !reflect.DeepEqual(back, want) {
			return text, "", false, errors.New("the edited claim does not read back as written")
		}
		return out, "updated", true, nil
	})
}

// llmBlock is a claim whose llm section is a block the switches can be
// written under.
var llmBlock = regexp.MustCompile(`(?m)^  llm:[ \t]*(#.*)?$`)

func modelsEditError(err error) error {
	if errors.Is(err, ErrCatalogueFlowForm) {
		return ErrModelsFlowForm
	}
	return err
}

// textScalar writes a string field so that YAML reads it back as that string:
// bare where that is so, quoted where it would be read as a number, a
// boolean, a comment or nothing.
func textScalar(v string) string { return pathScalar(v) }

func renderInstances(instances []ModelInstance) []string {
	if len(instances) == 0 {
		return []string{"    instances: []"}
	}
	out := []string{"    instances:"}
	for _, i := range instances {
		out = append(out, "      - name: "+textScalar(i.Name), "        modelId: "+textScalar(i.ModelID))
		for _, f := range []struct{ key, value string }{
			{"gpuMemoryUtilization", i.GPUMemoryUtilization},
			{"maxModelLen", i.MaxModelLen},
			{"modelCacheSize", i.ModelCacheSize},
			{"imageTag", i.ImageTag},
			{"toolCallParser", i.ToolCallParser},
		} {
			if f.value != "" {
				out = append(out, "        "+f.key+": "+textScalar(f.value))
			}
		}
	}
	return out
}

func renderProviders(providers []ModelProvider) []string {
	if len(providers) == 0 {
		return []string{"    providers: []"}
	}
	out := []string{"    providers:"}
	for _, p := range providers {
		out = append(out, "      - name: "+textScalar(p.Name))
		if p.DisplayName != "" {
			out = append(out, "        displayName: "+textScalar(p.DisplayName))
		}
		out = append(out, "        apiBase: "+textScalar(p.APIBase), "        apiKeyProperty: "+textScalar(p.APIKeyProperty))
		if len(p.Models) == 0 {
			out = append(out, "        models: []")
			continue
		}
		out = append(out, "        models:")
		for _, m := range p.Models {
			out = append(out, "          - name: "+textScalar(m.Name), "            model: "+textScalar(m.Model))
			if m.MaxTokens != nil {
				out = append(out, "            maxTokens: "+strconv.FormatInt(*m.MaxTokens, 10))
			}
			if m.Mode != "" {
				out = append(out, "            mode: "+textScalar(m.Mode))
			}
		}
	}
	return out
}
