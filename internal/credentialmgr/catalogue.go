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

package credentialmgr

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianv1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Field is one input a form must collect. Note what is absent: there is no
// Value. This type describes the shape of a credential, never its content.
type Field struct {
	Key       string `json:"key"`
	Format    string `json:"format,omitempty"`
	Secret    bool   `json:"secret"`
	MinLength int    `json:"minLength,omitempty"`
	Example   string `json:"example,omitempty"`
}

// Status is everything the API will say about one requirement.
//
// The read-back prohibition lives here as a type: there is no field capable of
// carrying a credential value, so no handler can accidentally serialise one.
type Status struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"displayName"`
	Description string  `json:"description,omitempty"`
	Phase       string  `json:"phase"`
	Scope       string  `json:"scope"`
	Optional    bool    `json:"optional"`
	VaultPath   string  `json:"vaultPath"`
	Fields      []Field `json:"fields"`
	Validator   string  `json:"validator,omitempty"`

	// Satisfied comes from ESO's sync status on the probe ExternalSecret, not
	// from asking OpenBao. Nothing here polls the secret store.
	Satisfied bool   `json:"satisfied"`
	Reason    string `json:"reason,omitempty"`

	// Metadata, when the caller's token can read it. Never a value.
	SetBy     string `json:"setBy,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// The credential manager reads the catalogue and ESO's verdict on it. Read-only
// on both: it never creates a requirement, and it never touches the Secret an
// ExternalSecret would produce.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=credentialrequirements,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch

// Catalogue reads CredentialRequirement objects and their ESO probes.
type Catalogue struct {
	Client client.Client
	// ProbeNamespace holds the probe ExternalSecrets the generator emits.
	ProbeNamespace string
}

// List returns the requirements visible to a caller with the given scopes.
//
// Scope filtering is a visibility decision, not an authorisation one — OpenBao
// still refuses a write the caller's policy forbids. But §9 is explicit that
// the asymmetry matters: showing a tenant admin a cluster-scoped form is an
// annoyance, while the inverse is a breach. So the default is the narrow set,
// and cluster scope has to be asked for.
func (c *Catalogue) List(ctx context.Context, scopes []string) ([]Status, error) {
	var reqs gentianv1alpha1.CredentialRequirementList
	if err := c.Client.List(ctx, &reqs); err != nil {
		return nil, fmt.Errorf("listing credential requirements: %w", err)
	}

	allowed := map[string]bool{}
	for _, s := range scopes {
		allowed[s] = true
	}

	out := make([]Status, 0, len(reqs.Items))
	for i := range reqs.Items {
		r := &reqs.Items[i]
		if !allowed[r.Spec.Scope] {
			continue
		}
		st := Status{
			Name:        r.Name,
			DisplayName: r.Spec.DisplayName,
			Description: r.Spec.Description,
			Phase:       r.Spec.Phase,
			Scope:       r.Spec.Scope,
			Optional:    r.Spec.Optional,
			VaultPath:   r.Spec.VaultPath,
		}
		for _, f := range r.Spec.Fields {
			st.Fields = append(st.Fields, Field{
				Key:       f.Key,
				Format:    f.Format,
				Secret:    f.Secret,
				MinLength: f.MinLength,
				Example:   f.Example,
			})
		}
		if r.Spec.Validate != nil {
			st.Validator = r.Spec.Validate.Type
		}
		st.Satisfied, st.Reason = c.probeStatus(ctx, r.Name)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one requirement, or an error the handler maps to 404.
func (c *Catalogue) Get(ctx context.Context, name string, scopes []string) (*Status, error) {
	all, err := c.List(ctx, scopes)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("no such requirement, or not visible at this scope: %s", name)
}

// probeStatus reads ESO's verdict from the probe ExternalSecret.
//
// Unstructured rather than a typed ESO client: adding external-secrets to
// go.mod for one condition read would couple the operator's build to ESO's API
// version, which has already moved once (v1beta1 stopped being served).
func (c *Catalogue) probeStatus(ctx context.Context, name string) (bool, string) {
	var es unstructured.Unstructured
	es.SetGroupVersionKind(externalSecretGVK)
	key := client.ObjectKey{Namespace: c.ProbeNamespace, Name: "credreq-" + name}

	if err := c.Client.Get(ctx, key, &es); err != nil {
		return false, "no satisfaction probe found — is the credential catalogue applied?"
	}
	conds, found, err := unstructured.NestedSlice(es.Object, "status", "conditions")
	if err != nil || !found {
		return false, "probe has not reported yet"
	}
	for _, raw := range conds {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == "Ready" {
			if cond["status"] == "True" {
				return true, ""
			}
			if msg, ok := cond["message"].(string); ok && msg != "" {
				return false, msg
			}
			return false, "not ready"
		}
	}
	return false, "probe has not reported a Ready condition"
}
