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
	// CredentialsSecret holds what the app instance and the arbiter share:
	// the datastore's URI for the one and the instance's preshared key for
	// both. The operator makes it once.
	CredentialsSecret = "openfga-apps-credentials"
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

// Table is what the operator writes: one entry per component that declares
// requires.services.authorization and holds its key.
type Table struct {
	Stores []Entry `json:"stores"`
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
	return t, nil
}
