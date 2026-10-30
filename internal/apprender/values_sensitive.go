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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The values a chart is told without their being release values: each one
// the profile maps is a key of one Secret of the app, <app>-sensitive-values,
// which an ExternalSecret fills from the tenant's store, and the release
// names each key with a set entry that takes its value from that Secret.

// valuesSensitive is what the app's ExternalSecret reads and what its
// release is told from it.
type valuesSensitive struct {
	// data are the ExternalSecret's entries, in the order the Composition
	// listed them.
	data []interface{}
	// set are the release's set entries.
	set []interface{}
	// documents are keys of the Secret templated from the others, each a
	// values document of its own, and valuesFrom the release's optional
	// reads of them.
	documents  map[string]interface{}
	valuesFrom []interface{}
}

func (v *valuesSensitive) read(secretKey, path, property string) {
	v.data = append(v.data, map[string]interface{}{
		"secretKey": secretKey,
		"remoteRef": map[string]interface{}{"key": path, "property": property},
	})
}

// sensitiveValues collects every value the profile maps from the tenant's
// store.
func (r *valuesRender) sensitiveValues() *valuesSensitive {
	v := &valuesSensitive{documents: map[string]interface{}{}}
	vm := r.vm
	secret, ns := r.secretName(), r.ns
	set := func(name, key string) {
		if name == "" {
			return
		}
		v.set = append(v.set, map[string]interface{}{
			"name": name,
			"valueFrom": map[string]interface{}{
				"secretKeyRef": map[string]interface{}{"name": secret, "namespace": ns, "key": key},
			},
		})
	}
	read := func(mapped string, secretKey, path, property string) {
		if mapped != "" {
			v.read(secretKey, path, property)
		}
	}
	// The model gateway's record is written for a profile that declares
	// requires.services.llm and for no other; an entry naming a path that is
	// never written fails on every refresh.
	llm := vm.LLM != nil && r.services().LLM != nil && !r.in.Placed
	// A placed component's database is the operator's to provide, and so is
	// its way to the model gateway: neither is read from the tenant's store.
	database := vm.Database != nil && !r.in.Placed
	integrations := make([]string, 0, len(vm.Integrations))
	for contract := range vm.Integrations {
		integrations = append(integrations, contract)
	}
	sort.Strings(integrations)

	// The ExternalSecret's entries.
	if m := vm.OIDC; m != nil {
		path := r.appPath("oidc")
		read(m.IssuerKey, "oidc-issuer", path, "issuer")
		read(m.ClientIDKey, "oidc-client-id", path, "client-id")
		if m.ClientSecretKey != "" {
			// A client in the kernel realm names its own path and property.
			csPath, csProp := path, "client-secret"
			if m.ClientSecretOpenbaoPath != "" {
				csPath = m.ClientSecretOpenbaoPath
			}
			if m.ClientSecretOpenbaoProperty != "" {
				csProp = m.ClientSecretOpenbaoProperty
			}
			v.read("oidc-client-secret", csPath, csProp)
		}
	}
	if m := vm.Database; database {
		path := r.appPath("database")
		read(m.HostKey, "db-host", path, "host")
		read(m.PortKey, "db-port", path, "port")
		// The read-only name is the same database's name: read for either
		// key, or a set entry for the read-only one names a key the Secret
		// never holds.
		if m.NameKey != "" || m.ReadNameKey != "" {
			v.read("db-name", path, "name")
		}
		read(m.UserKey, "db-user", path, "user")
		read(m.PasswordKey, "db-password", path, "password")
	}
	if m := vm.S3; m != nil {
		path := r.appPath("s3")
		read(m.EndpointKey, "s3-endpoint", path, "endpoint")
		read(m.BucketKey, "s3-bucket", path, "bucket")
		read(m.AccessKeyKey, "s3-access-key", path, "access-key")
		read(m.SecretKeyKey, "s3-secret-key", path, "secret-key")
		read(m.RegionKey, "s3-region", path, "region")
	}
	if m := vm.Cache; m != nil {
		path := r.appPath("cache")
		read(m.HostKey, "cache-host", path, "host")
		read(m.PortKey, "cache-port", path, "port")
		read(m.UserKey, "cache-user", path, "user")
		read(m.PasswordKey, "cache-password", path, "password")
	}
	if llm {
		path := r.appPath("llm")
		read(vm.LLM.BaseURLKey, "llm-base-url", path, "base-url")
		read(vm.LLM.APIKeyKey, "llm-api-key", path, "api-key")
	}
	if m := vm.SMTP; m != nil {
		path := r.appPath("smtp")
		read(m.HostKey, "smtp-host", path, "host")
		read(m.PortKey, "smtp-port", path, "port")
		read(m.UserKey, "smtp-user", path, "user")
		read(m.PasswordKey, "smtp-password", path, "password")
	}
	if m := vm.IMAP; m != nil {
		path := r.appPath("imap")
		read(m.HostKey, "imap-host", path, "host")
		read(m.PortKey, "imap-port", path, "port")
	}
	for _, contract := range integrations {
		m := vm.Integrations[contract]
		path := fmt.Sprintf("gentian-os/tenants/%s/contracts/%s", r.tenant, contract)
		read(m.EndpointKey, "integration-"+contract+"-endpoint", path, "endpoint")
		read(m.UserKey, "integration-"+contract+"-user", path, "user")
		read(m.PasswordKey, "integration-"+contract+"-password", path, "password")
	}
	for _, s := range r.generated() {
		if s.Name != "" && s.ValuePath != "" {
			v.read("internal-"+s.Name, r.appPath("internal/"+s.Name), "value")
		}
	}
	// A supplied credential is read from where the custodian stores what a
	// person typed in; the Secret is not made until every field is there,
	// so the release waits for the person.
	for _, sc := range r.supplied() {
		for _, f := range sc.Fields {
			if f.Key != "" && f.ValuePath != "" {
				v.read("supplied-"+sc.Name+"-"+f.Key, r.appPath("supplied/"+sc.Name), f.Key)
			}
		}
	}

	// The release's set entries, in the Composition's order: a database's
	// read-only name and user are the same keys as its own.
	if m := vm.OIDC; m != nil {
		set(m.IssuerKey, "oidc-issuer")
		set(m.ClientIDKey, "oidc-client-id")
		set(m.ClientSecretKey, "oidc-client-secret")
	}
	if m := vm.Database; database {
		set(m.HostKey, "db-host")
		set(m.PortKey, "db-port")
		set(m.NameKey, "db-name")
		set(m.ReadNameKey, "db-name")
		set(m.UserKey, "db-user")
		set(m.ReadUserKey, "db-user")
		set(m.PasswordKey, "db-password")
	}
	if m := vm.S3; m != nil {
		set(m.EndpointKey, "s3-endpoint")
		set(m.BucketKey, "s3-bucket")
		set(m.AccessKeyKey, "s3-access-key")
		set(m.SecretKeyKey, "s3-secret-key")
		set(m.RegionKey, "s3-region")
	}
	if m := vm.Cache; m != nil {
		set(m.HostKey, "cache-host")
		set(m.PortKey, "cache-port")
		set(m.UserKey, "cache-user")
		set(m.PasswordKey, "cache-password")
	}
	if llm {
		set(vm.LLM.BaseURLKey, "llm-base-url")
		set(vm.LLM.APIKeyKey, "llm-api-key")
	}
	// Mail is optional: provider-helm refuses an empty set value, which a
	// cluster without the relay's credentials has, so SMTP goes through
	// optional valuesFrom documents instead.
	if m := vm.SMTP; m != nil {
		v.optional(secret, ns, m.HostKey, "smtp-host", false)
		v.optional(secret, ns, m.PortKey, "smtp-port", true)
		v.optional(secret, ns, m.UserKey, "smtp-user", false)
		v.optional(secret, ns, m.PasswordKey, "smtp-password", false)
	}
	if m := vm.IMAP; m != nil {
		set(m.HostKey, "imap-host")
		set(m.PortKey, "imap-port")
	}
	for _, contract := range integrations {
		m := vm.Integrations[contract]
		set(m.EndpointKey, "integration-"+contract+"-endpoint")
		set(m.UserKey, "integration-"+contract+"-user")
		set(m.PasswordKey, "integration-"+contract+"-password")
	}
	for _, s := range r.generated() {
		if s.Name != "" && s.ValuePath != "" {
			set(s.ValuePath, "internal-"+s.Name)
		}
	}
	for _, sc := range r.supplied() {
		for _, f := range sc.Fields {
			if f.Key != "" && f.ValuePath != "" {
				set(f.ValuePath, "supplied-"+sc.Name+"-"+f.Key)
			}
		}
	}
	return v
}

// optional adds a value the chart may go without: templated into the Secret
// as a values document of its own when it is not empty, and read by the
// release as optional valuesFrom. raw writes the value as it is (a port)
// rather than as a JSON string.
func (v *valuesSensitive) optional(secret, ns, valuePath, secretKey string, raw bool) {
	if valuePath == "" {
		return
	}
	var head, tail strings.Builder
	for _, part := range strings.Split(valuePath, ".") {
		quoted, _ := json.Marshal(part)
		head.WriteString("{" + string(quoted) + ":")
		tail.WriteString("}")
	}
	value := " . | toJson "
	if raw {
		value = " . "
	}
	docKey := "values-" + secretKey
	v.documents[docKey] = fmt.Sprintf("{{ with index . %q }}%s{{%s}}%s{{ end }}", secretKey, head.String(), value, tail.String())
	v.valuesFrom = append(v.valuesFrom, map[string]interface{}{
		"secretKeyRef": map[string]interface{}{"name": secret, "namespace": ns, "key": docKey, "optional": true},
	})
}

// sensitiveSecret is the ExternalSecret that fills the app's Secret from
// the tenant's own store, nil when the profile maps nothing.
func (r *valuesRender) sensitiveSecret(v *valuesSensitive) *unstructured.Unstructured {
	if len(v.data) == 0 {
		return nil
	}
	es := valuesObject("external-secrets.io/v1", "ExternalSecret", r.secretName(), r.ns)
	target := map[string]interface{}{
		"name":           r.secretName(),
		"creationPolicy": "Owner",
		"deletionPolicy": "Retain",
	}
	if len(v.documents) > 0 {
		target["template"] = map[string]interface{}{
			"engineVersion": "v2",
			"mergePolicy":   "Merge",
			"data":          v.documents,
		}
	}
	es.Object["spec"] = map[string]interface{}{
		"refreshInterval": "1h",
		// The tenant's own store: the kernel's is not usable from a
		// tenant's namespace.
		"secretStoreRef": map[string]interface{}{
			"kind": "ClusterSecretStore",
			"name": "openbao-tenant-" + r.tenant,
		},
		"target": target,
		"data":   v.data,
	}
	return es
}
