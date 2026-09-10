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
	Tenant      string  `json:"tenant,omitempty"`
	Optional    bool    `json:"optional"`
	VaultPath   string  `json:"vaultPath"`
	Fields      []Field `json:"fields"`
	Validator   string  `json:"validator,omitempty"`
	// ValidateHost is the endpoint the validator probes, when the requirement
	// declares one. Not a credential — an OCI registry or a git remote's own
	// address — so exposing it alongside VaultPath carries the same weight.
	ValidateHost string `json:"validateHost,omitempty"`

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

// Viewer is the verified identity a listing is filtered against.
//
// It is produced from OpenBao's verdict on the caller's token, never from
// anything the caller states about itself — see identify in http.go.
type Viewer struct {
	// ClusterAdmin widens the listing to cluster-scoped requirements.
	ClusterAdmin bool
	// Tenant is the single tenant whose requirements this caller may see. Empty
	// for a caller with no tenant, who therefore sees no tenant-scoped entry.
	Tenant string
}

// List returns the requirements visible to one viewer.
//
// Filtering is a visibility decision, not an authorisation one — OpenBao still
// refuses a write the caller's policy forbids. But the asymmetry matters:
// showing a tenant admin a cluster-scoped form is an annoyance, while the
// inverse is a breach. So visibility is granted, never assumed.
//
// Tenant scope is matched on IDENTITY, not on class. A tenant-scoped
// requirement is visible only to its own tenant, because "every tenant admin
// can see every tenant's repository credential" is the failure this exists to
// prevent. A cluster admin sees everything, which is what makes the admin panel
// useful.
func (c *Catalogue) List(ctx context.Context, v Viewer) ([]Status, error) {
	var reqs gentianv1alpha1.CredentialRequirementList
	if err := c.Client.List(ctx, &reqs); err != nil {
		return nil, fmt.Errorf("listing credential requirements: %w", err)
	}

	out := make([]Status, 0, len(reqs.Items))
	for i := range reqs.Items {
		r := &reqs.Items[i]
		if !v.canSee(r.Spec.Scope, r.Spec.Tenant) {
			continue
		}
		st := Status{
			Name:        r.Name,
			DisplayName: r.Spec.DisplayName,
			Description: r.Spec.Description,
			Phase:       r.Spec.Phase,
			Scope:       r.Spec.Scope,
			Tenant:      r.Spec.Tenant,
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
			st.ValidateHost = r.Spec.Validate.Host
		}
		st.Satisfied, st.Reason = c.probeStatus(ctx, r.Name)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// canSee decides whether one requirement is visible to this viewer.
//
// Written as an explicit allow-list rather than a set of exclusions: a scope
// value this function does not recognise is invisible, so adding a scope to the
// enum without teaching this function about it hides requirements rather than
// exposing them.
func (v Viewer) canSee(scope, tenant string) bool {
	switch scope {
	case "cluster":
		return v.ClusterAdmin
	case "tenant":
		if v.ClusterAdmin {
			return true
		}
		// A requirement with no tenant is visible to nobody. The CRD's CEL rule
		// rejects that combination at admission; this is the second line, for
		// objects that predate the rule.
		return tenant != "" && tenant == v.Tenant
	default:
		return false
	}
}

// Get returns one requirement, or an error the handler maps to 404.
func (c *Catalogue) Get(ctx context.Context, name string, v Viewer) (*Status, error) {
	all, err := c.List(ctx, v)
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
