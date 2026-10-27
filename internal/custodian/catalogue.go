/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianv1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ProviderLabel names who issues a requirement's credential; the catalogue
// generator sets it from kernel/platforms.yaml and credentials.yaml.
const ProviderLabel = "gentianos.io/credential-provider"

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
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	Phase       string `json:"phase"`
	Scope       string `json:"scope"`
	Tenant      string `json:"tenant,omitempty"`
	// Provider is who issues the credential (cloudflare, infomaniak, aws...),
	// from the catalogue's gentianos.io/credential-provider label, so a screen
	// can keep one vendor's tokens together. Empty for the platform's own.
	Provider  string  `json:"provider,omitempty"`
	Optional  bool    `json:"optional"`
	VaultPath string  `json:"vaultPath"`
	Fields    []Field `json:"fields"`
	Validator string  `json:"validator,omitempty"`
	// ValidateHost is the endpoint the validator probes, when the requirement
	// declares one. Not a credential — an OCI registry or a git remote's own
	// address — so exposing it alongside VaultPath carries the same weight.
	ValidateHost string `json:"validateHost,omitempty"`

	// Satisfied comes from ESO's sync status on the probe ExternalSecret, not
	// from asking OpenBao. Nothing here polls the secret store.
	Satisfied bool   `json:"satisfied"`
	Reason    string `json:"reason,omitempty"`
	// Pending is a value stored moments ago that ESO has not confirmed yet:
	// the probe was told to re-read it and has not answered. Never true
	// together with Satisfied, and only for pendingWindow after the write --
	// past that, ESO's own verdict stands, whatever it is.
	Pending bool `json:"pending,omitempty"`

	// Metadata, when the caller's token can read it. Never a value.
	SetBy     string `json:"setBy,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// The custodian reads the catalogue and ESO's verdict on it. Read-only
// on both: it never creates a requirement, and it never touches the Secret an
// ExternalSecret would produce.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=credentialrequirements,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;patch

// Catalogue reads CredentialRequirement objects and their ESO probes.
type Catalogue struct {
	Client client.Client
	// ProbeNamespace holds the probe ExternalSecrets the generator emits.
	ProbeNamespace string
	// Now is the clock pending is measured against; nil is time.Now.
	Now func() time.Time
}

// pendingWindow is how long after a write an unconfirmed probe reads as
// pending rather than missing. ESO answers a force-sync in seconds; this is
// room for a busy controller, short enough that a value ESO cannot read is
// reported as such while the person who stored it is still looking.
const pendingWindow = 2 * time.Minute

// Viewer is what one verified caller may see and set.
//
// It is the authorization store's answer about the caller, never anything the
// caller states about itself and never OpenBao's opinion of their token -- see
// viewOf in authorize.go.
type Viewer struct {
	// ClusterAdmin is can_write_credential on the cluster.
	ClusterAdmin bool
	// Tenant is the tenant whose realm signed the caller in, when they may
	// write its credentials. Empty for everybody else, who therefore have no
	// tenant to act for.
	Tenant string
	// check asks the store about one scope, remembered for the request. Nil
	// allows nothing.
	check func(relation, scope, tenant string) bool
}

// List returns the requirements visible to one viewer.
//
// Each requirement is shown to whoever holds can_read_credential on the scope
// it belongs to -- the cluster, or its one tenant -- and to nobody else.
// Tenant scope is matched on the tenant itself, not on being a tenant
// administrator somewhere: "every tenant admin can see every tenant's
// repository credential" is the failure this exists to prevent. A platform
// administrator sees a tenant's through the store's own derivation, for as
// long as the tenant consents to being operated.
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
			Provider:    r.Labels[ProviderLabel],
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
		st.Satisfied, st.Pending, st.Reason = c.probeStatus(ctx, r.Name)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// canSee reports whether the caller may read the credentials of a scope:
// that one is required, whether it is set, and who set it. Never its value.
//
// A tenant-scoped requirement with no tenant is visible to nobody. The CRD's
// CEL rule rejects that combination at admission; this is the second line,
// for objects that predate the rule.
func (v Viewer) canSee(scope, tenant string) bool {
	return v.check != nil && v.check(relationRead, scope, tenant)
}

// canWrite reports whether the caller may set the credentials of a scope.
func (v Viewer) canWrite(scope, tenant string) bool {
	return v.check != nil && v.check(relationWrite, scope, tenant)
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

// probeStatus reads ESO's verdict from the probe ExternalSecret: satisfied,
// pending, and the reason when it is neither.
//
// Pending covers the seconds between a write and ESO's answer. The custodian
// stamps the probe's force-sync annotation when it stores a value (see
// refreshConsumers); until the Ready condition has moved since that stamp, the
// condition on the object is ESO's opinion of the previous value, and reporting
// it as "missing" told the person who had just supplied it that it was lost.
//
// Unstructured rather than a typed ESO client: adding external-secrets to
// go.mod for one condition read would couple the operator's build to ESO's API
// version, which has already moved once (v1beta1 stopped being served).
func (c *Catalogue) probeStatus(ctx context.Context, name string) (satisfied, pending bool, reason string) {
	var es unstructured.Unstructured
	es.SetGroupVersionKind(externalSecretGVK)
	key := client.ObjectKey{Namespace: c.ProbeNamespace, Name: "credreq-" + name}

	if err := c.Client.Get(ctx, key, &es); err != nil {
		return false, false, "no satisfaction probe found — is the credential catalogue applied?"
	}
	satisfied, since, reason := readyCondition(&es)
	if satisfied {
		return true, false, ""
	}
	if c.awaitingProbe(&es, since) {
		return false, true, "saved, being checked"
	}
	return false, false, reason
}

// readyCondition is ESO's Ready condition: whether it is True, when it last
// changed (zero when unknown), and its message.
func readyCondition(es *unstructured.Unstructured) (bool, time.Time, string) {
	conds, found, err := unstructured.NestedSlice(es.Object, "status", "conditions")
	if err != nil || !found {
		return false, time.Time{}, "probe has not reported yet"
	}
	for _, raw := range conds {
		cond, ok := raw.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		var since time.Time
		if at, ok := cond["lastTransitionTime"].(string); ok {
			since, _ = time.Parse(time.RFC3339, at)
		}
		if cond["status"] == "True" {
			return true, since, ""
		}
		if msg, ok := cond["message"].(string); ok && msg != "" {
			return false, since, msg
		}
		return false, since, "not ready"
	}
	return false, time.Time{}, "probe has not reported a Ready condition"
}

// awaitingProbe reports whether the probe was nudged within pendingWindow and
// its Ready condition has not changed since. A condition that turned False
// after the nudge is ESO's answer about the new value, not a wait.
func (c *Catalogue) awaitingProbe(es *unstructured.Unstructured, readySince time.Time) bool {
	stamp, err := strconv.ParseInt(es.GetAnnotations()[forceSyncAnnotation], 10, 64)
	if err != nil {
		return false
	}
	nudged := time.Unix(stamp, 0)
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if now().Sub(nudged) > pendingWindow {
		return false
	}
	// Second resolution on both sides; a transition in the same second as the
	// nudge is taken as the answer to it.
	return readySince.IsZero() || readySince.Before(nudged)
}
