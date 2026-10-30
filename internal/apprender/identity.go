/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// renderIdentity registers the app at the tenant's realm: its entitlement
// group, its own OIDC client with what hangs off it, each extension's own
// client, and the sign-in sidecar's SAML client.
func renderIdentity(in *Input, out *Rendered) error {
	id := newIdentityScope(in)
	// The entitlement group is how a tenant's administrator gives people an
	// app they installed. Who may use a component the platform places is
	// the platform's to say, and it has none.
	if !in.Placed {
		out.add(id.entitlementGroup())
	}
	out.add(id.appClient()...)
	out.add(id.extensionClients()...)
	out.add(id.signInSidecar()...)
	return nil
}

// identityScope holds what every identity object reads, worked out once from
// the Input as the Composition worked it out at its top.
type identityScope struct {
	in      *Input
	profile *gentianov1alpha1.ComponentProfileSpec
	tenant  string
	realm   string
	domain  string
	// kernelDomain is gentian-kernel-services' KERNEL_DOMAIN. Empty puts
	// nothing in place of ${KERNEL_DOMAIN}, as the Composition did.
	kernelDomain string
	// provNs is where the client secrets' ExternalSecrets are written and
	// read back by provider-keycloak: the namespace gentian-cluster-config
	// is in.
	provNs string
	// packs are the profile's OIDC packs by client id, and mapperTemplates
	// the catalog's mapper templates by name.
	packs           map[string]gentianov1alpha1.OIDCPackSpec
	mapperTemplates map[string]gentianov1alpha1.OIDCMapperTemplate
}

func newIdentityScope(in *Input) *identityScope {
	id := &identityScope{
		in:           in,
		profile:      in.Profile,
		tenant:       in.Tenant(),
		realm:        in.RealmName(),
		domain:       in.TenantDomain(),
		kernelDomain: in.KernelServices["KERNEL_DOMAIN"],
		provNs:       in.ClusterConfigNamespace,
	}
	if id.provNs == "" {
		id.provNs = "kernel-provisioning"
	}
	if in.Packs != nil {
		id.packs = in.Packs.Packs
		id.mapperTemplates = in.Packs.MapperTemplates
	}
	return id
}

// services are the profile's requires.services, never nil.
func (id *identityScope) services() *gentianov1alpha1.ServiceRequirements {
	if id.profile.Requires == nil || id.profile.Requires.Services == nil {
		return &gentianov1alpha1.ServiceRequirements{}
	}
	return id.profile.Requires.Services
}

// place puts the tenant's and the kernel's domains into an address.
func (id *identityScope) place(uri string) string {
	uri = strings.ReplaceAll(uri, "${TENANT_DOMAIN}", id.domain)
	return strings.ReplaceAll(uri, "${KERNEL_DOMAIN}", id.kernelDomain)
}

func (id *identityScope) placeAll(uris []string) []string {
	var out []string
	for _, uri := range uris {
		out = append(out, id.place(uri))
	}
	return out
}

// name is a realm object's name: the tenant's, then the app's, then what it
// is. The realm objects are cluster-scoped, so the tenant makes them unique.
func (id *identityScope) name(suffix string) string {
	return id.tenant + "-" + id.in.App + "-" + suffix
}

// identityGroupName turns a Keycloak group path into the object name the
// group is declared under (the groups Job's and tenant-default's rule).
func identityGroupName(path string) string {
	return strings.NewReplacer(":", "-", "_", "-", ".", "-").Replace(strings.ToLower(path))
}

// entitlementGroup is the app's entitlement group.
//
// Outside the OIDC client on purpose: an entitlement group is per app, not
// per OIDC client, and a profile with no oidc block still has one.
//
// Here rather than in tenant-default because its attributes come from the
// profile's gentianos.io/keycloak-group-attributes annotation, and a Group
// declared without them would strip what the groups Job sets. Values are
// strings: provider-keycloak splits a value on "##" into Keycloak's list, so
// a list in the annotation is joined with "##". And the platform's mark,
// which a realm import reads: a group carrying it is never made from a
// bundle (internal/keycloak PlatformMarkerAttribute).
func (id *identityScope) entitlementGroup() *unstructured.Unstructured {
	path := fmt.Sprintf("gentian:tenant:%s:app:%s", id.tenant, id.in.App)
	attrs := map[string]interface{}{}
	if raw := id.in.ProfileAnnotations["gentianos.io/keycloak-group-attributes"]; raw != "" {
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(raw), &parsed) == nil {
			for k, v := range parsed {
				attrs[k] = strings.Join(identityStrings(v), "##")
			}
		}
	}
	attrs["gentianos.io/managed-by"] = "platform"
	return identityObject("group.keycloak.crossplane.io/v1alpha1", "Group", identityGroupName(path), map[string]interface{}{
		"deletionPolicy": "Orphan",
		"forProvider": map[string]interface{}{
			"realmId":    id.realm,
			"name":       path,
			"attributes": attrs,
		},
		"providerConfigRef": identityProvider,
	})
}

// identityStrings reads a JSON value as a list of strings, the way the
// Composition's join read it: a list item by item, anything else as one.
func identityStrings(v interface{}) []string {
	str := func(v interface{}) string {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	switch v := v.(type) {
	case nil:
		return nil
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if item != nil {
				out = append(out, str(item))
			}
		}
		return out
	default:
		return []string{str(v)}
	}
}

// identityProvider is the provider config every realm object is written
// through.
var identityProvider = map[string]interface{}{"name": "keycloak"}

// identityObject builds a cluster-scoped object.
func identityObject(apiVersion, kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name},
		"spec":       spec,
	}}
	return obj
}

// identityRef refers to another managed object by name, resolved on every
// reconcile: a client recreated under a new id would otherwise leave the
// reference naming a dead one.
func identityRef(name string) map[string]interface{} {
	return map[string]interface{}{
		"name":   name,
		"policy": map[string]interface{}{"resolve": "Always"},
	}
}

// identityManaged is every management policy except LateInitialize. The
// default "*" includes it, and late-init copies fields this render does not
// set out of the live client into its declared intent; with a second writer
// (the OIDC pack Job configures the same clientId) that writer's values
// silently become this object's, as they did for the portal BFF client,
// which then sat Synced=False unable to reconcile while reading Ready=True.
var identityManaged = []interface{}{"Observe", "Create", "Update", "Delete"}

// identityClientCreds is the ExternalSecret provider-keycloak reads a
// confidential client's secret from, at the OpenBao path the app's own
// ExternalSecret reads it from too.
func (id *identityScope) clientCreds(name, openbaoPath string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "external-secrets.io/v1",
		"kind":       "ExternalSecret",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": id.provNs,
		},
		"spec": map[string]interface{}{
			"refreshInterval": "1h",
			"secretStoreRef": map[string]interface{}{
				"kind": "ClusterSecretStore",
				"name": "openbao",
			},
			"target": map[string]interface{}{
				"name":           name,
				"creationPolicy": "Owner",
			},
			"data": []interface{}{
				map[string]interface{}{
					"secretKey": "client-secret",
					"remoteRef": map[string]interface{}{
						"key":      openbaoPath,
						"property": "client-secret",
					},
				},
			},
		},
	}}
}

// audienceMapper puts one name in the aud of a client's access tokens. A
// custom audience, because what is named is a service and need not be a
// client of this realm, and Keycloak drops a client audience whose client
// does not exist. Delete, unlike the mappers of a client that goes as a
// whole: the client stays when a profile stops naming an audience, and the
// mapper is what has to go then.
func (id *identityScope) audienceMapper(key, client, aud string) *unstructured.Unstructured {
	return identityObject("client.keycloak.crossplane.io/v1alpha1", "ProtocolMapper",
		id.tenant+"-"+key+"-oidc-audience-"+aud, map[string]interface{}{
			"deletionPolicy": "Delete",
			"forProvider": map[string]interface{}{
				"realmId":        id.realm,
				"clientIdRef":    identityRef(client),
				"name":           "audience-" + aud,
				"protocol":       "openid-connect",
				"protocolMapper": "oidc-audience-mapper",
				"config": map[string]interface{}{
					"included.custom.audience":  aud,
					"id.token.claim":            "false",
					"access.token.claim":        "true",
					"introspection.token.claim": "true",
					"userinfo.token.claim":      "false",
				},
			},
			"providerConfigRef": identityProvider,
		})
}

func identityStringList(s []string) []interface{} {
	out := make([]interface{}, 0, len(s))
	for _, v := range s {
		out = append(out, v)
	}
	return out
}
