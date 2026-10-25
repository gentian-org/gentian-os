/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// What the operator and the arbiter agree on: where the arbiter runs, the
// table the operator writes for it and the ConfigMap the arbiter keeps its
// store ids in. Both sides import these, so neither can name another.
const (
	// Namespace is the system namespace the arbiter and the app instance of
	// OpenFGA run in.
	Namespace = "system-authorization"
	// ServiceName is the arbiter's Service, and the name of its Deployment,
	// ServiceAccount and pods' app label.
	ServiceName = "arbiter"
	// Port is the arbiter's one listener: the API components call. The
	// Service answers on the same port, so that a NetworkPolicy, which
	// Calico matches after the Service's translation, names one number.
	Port = int32(8080)
	// HealthPort answers the kubelet's probes and nothing else.
	HealthPort = int32(8081)

	// TableConfigMap is the operator's table, in Namespace, under TableKey.
	TableConfigMap = "arbiter-stores"
	TableKey       = "stores.json"
	// StoresConfigMap is where the arbiter records the stores and models it
	// created, in Namespace. The operator never writes it.
	StoresConfigMap = "arbiter-store-ids"

	// InstanceRelease is the Helm release of the app instance of OpenFGA:
	// its Service and its pods' instance label.
	InstanceRelease = "openfga-apps"
	// DatastoreSecret holds the app instance's datastore URI under the key
	// `uri`, its database and role on the tenants' PostgreSQL. The operator
	// makes it once, with the role.
	DatastoreSecret = "openfga-apps-datastore"
	// KeySecret holds the app instance's preshared key under the key
	// `keys`. The arbiter makes it, on its first start, and is the only
	// holder of it beside the instance: the key opens every store.
	KeySecret = "openfga-apps-key"
)

// Entry is one store of the table: a tenant's component, the hash of the
// key that opens its store, and the model the store is decided against.
type Entry struct {
	Tenant    string `json:"tenant"`
	Component string `json:"component"`
	// KeyHash is the hex SHA-256 of the component's key. The key itself is
	// only ever in the component's Secret.
	KeyHash string `json:"keyHash"`
	// Model is the compiled model (Compile), as OpenFGA's API takes it.
	Model json.RawMessage `json:"model"`
}

// Ref names a store by its tenant and component.
type Ref struct {
	Tenant    string `json:"tenant"`
	Component string `json:"component"`
}

// Table is what the operator writes: one entry per component that declares
// requires.services.authorization and holds its key, and the stores a purge
// has destroyed the rest of an app's data for, which the arbiter deletes.
type Table struct {
	Stores []Entry `json:"stores"`
	Purge  []Ref   `json:"purge,omitempty"`
}

// dnsLabel is what a tenant's and a component's name are.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// StoreName is the name a store is created under, <tenant>/<component>: for
// a person reading OpenFGA, never for finding it again, which is by the id
// the arbiter recorded.
func (e Entry) StoreName() string { return e.Tenant + "/" + e.Component }

// recordKey is the key of an entry's ids in StoresConfigMap. A dot is in
// neither name, so no two entries share one.
func (e Entry) recordKey() string { return e.Tenant + "." + e.Component }

// LoadTable reads the table the operator wrote. An entry that names no
// tenant, no component, no key or no model is refused with the whole table:
// a table the operator did not write is not one to decide by.
func LoadTable(path string) (*Table, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseTable(raw)
}

// ParseTable is LoadTable for bytes already read.
func ParseTable(raw []byte) (*Table, error) {
	t := &Table{}
	if err := json.Unmarshal(raw, t); err != nil {
		return nil, fmt.Errorf("the arbiter's table: %w", err)
	}
	seen := map[string]bool{}
	for _, e := range t.Stores {
		if !dnsLabel.MatchString(e.Tenant) || !dnsLabel.MatchString(e.Component) || len(e.KeyHash) != 64 || len(e.Model) == 0 {
			return nil, fmt.Errorf("the arbiter's table holds an entry that is not one: %q/%q", e.Tenant, e.Component)
		}
		if seen[e.KeyHash] || seen[e.recordKey()] {
			return nil, fmt.Errorf("the arbiter's table names %s or its key twice", e.StoreName())
		}
		seen[e.KeyHash], seen[e.recordKey()] = true, true
	}
	for _, p := range t.Purge {
		e := Entry{Tenant: p.Tenant, Component: p.Component}
		if !dnsLabel.MatchString(p.Tenant) || !dnsLabel.MatchString(p.Component) {
			return nil, fmt.Errorf("the arbiter's table purges a store that is not one: %q/%q", p.Tenant, p.Component)
		}
		// A store is decided in or purged, never both: a table that says
		// both is not one the operator wrote.
		if seen[e.recordKey()] {
			return nil, fmt.Errorf("the arbiter's table both keeps and purges %s", e.StoreName())
		}
	}
	return t, nil
}
