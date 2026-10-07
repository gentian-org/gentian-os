/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The record of what was provisioned.
//
// Which stores an app has is declared by its profile, and that is enough
// while the app is installed. It stops being enough the moment the app is
// uninstalled: the app has left the tenant's manifest, its stores are all
// still there, and the acts that come afterwards -- the read of what
// uninstalled apps hold, a purge, the deletion of the tenant -- have to find
// them. They used to find a PostgreSQL database through the Database object
// the cluster keeps, and a MariaDB database, a bucket and a cache user only
// through the Jobs that had set them up, which expire. A tenant deleted a day
// after an app was uninstalled left that app's bucket behind and reported
// itself deleted.
//
// So provisioning writes down what it made: one ConfigMap per tenant, one
// entry per app, written before the Jobs that create the stores are handed
// over and never removed by provisioning. An entry goes kind by kind as a
// purge destroys each store, and the whole record goes last when the tenant
// is deleted with deletionPolicy: Delete. With Retain it stays, because the
// stores do.

// ProvisionedConfigType labels the record, beside the tenant's label.
const ProvisionedConfigType = "tenant-provisioned-stores"

// ConfigTypeLabel is the label the operator's per-tenant ConfigMaps say
// what they are with.
const ConfigTypeLabel = "gentianos.io/config-type"

// ProvisionedRecordKey is where a tenant's record is kept: in the
// provisioning namespace, beside the manifests the tenant is provisioned
// from, and outside the tenant's own namespace, which a deletion removes
// before it is done reading this.
func ProvisionedRecordKey(tenantName string) types.NamespacedName {
	return types.NamespacedName{
		Namespace: layout.Namespace(layout.Provisioning),
		Name:      "tenant-" + tenantName + "-provisioned-stores",
	}
}

// Provisioned is what was provisioned for one app: the stores, by the names
// they were made under.
type Provisioned struct {
	// DatabaseEngine is the engine of the app's database, "" without one.
	DatabaseEngine gentianov1alpha1.DatabaseEngine `json:"databaseEngine,omitempty"`
	// Database and DatabaseUser are the database and its login.
	Database     string `json:"database,omitempty"`
	DatabaseUser string `json:"databaseUser,omitempty"`
	// Bucket is the object-storage bucket, "" without one.
	Bucket string `json:"bucket,omitempty"`
	// CacheUser is the user in the shared cache, "" without one.
	CacheUser string `json:"cacheUser,omitempty"`
	// ModelKey is the alias of the key registered for the app at the model
	// gateway, "" when none was: on a cluster that serves no models.
	ModelKey string `json:"modelKey,omitempty"`
}

// ProvisionedOf is the record entry of an inventory.
func ProvisionedOf(inv AppInventory) Provisioned {
	return Provisioned{
		DatabaseEngine: inv.Stores.Database,
		Database:       inv.Database,
		DatabaseUser:   inv.DatabaseUser,
		Bucket:         inv.Bucket,
		CacheUser:      inv.CacheUser,
	}
}

// Stores are the stores the entry records.
func (p Provisioned) Stores() Stores {
	return Stores{Database: p.DatabaseEngine, S3: p.Bucket != "", Redis: p.CacheUser != ""}
}

// Empty reports whether the entry records nothing.
func (p Provisioned) Empty() bool {
	return p == Provisioned{}
}

// Has reports whether the entry records a store of the kind.
func (p Provisioned) Has(kind Kind) bool {
	switch kind {
	case KindDatabase:
		return p.DatabaseEngine != ""
	case KindObjectStorage:
		return p.Bucket != ""
	case KindCache:
		return p.CacheUser != ""
	case KindModelAccess:
		return p.ModelKey != ""
	}
	return false
}

// Without is the entry with one kind taken out: what is left to record once
// that store has been destroyed.
func (p Provisioned) Without(kind Kind) Provisioned {
	switch kind {
	case KindDatabase:
		p.DatabaseEngine, p.Database, p.DatabaseUser = "", "", ""
	case KindObjectStorage:
		p.Bucket = ""
	case KindCache:
		p.CacheUser = ""
	case KindModelAccess:
		p.ModelKey = ""
	}
	return p
}

// merged adds what another entry records to this one. Provisioning only
// ever adds: a profile that stops declaring a bucket has not removed the
// bucket.
func (p Provisioned) merged(with Provisioned) Provisioned {
	if with.DatabaseEngine != "" {
		p.DatabaseEngine, p.Database, p.DatabaseUser = with.DatabaseEngine, with.Database, with.DatabaseUser
	}
	if with.Bucket != "" {
		p.Bucket = with.Bucket
	}
	if with.CacheUser != "" {
		p.CacheUser = with.CacheUser
	}
	if with.ModelKey != "" {
		p.ModelKey = with.ModelKey
	}
	return p
}

// MergeStores is the stores an act on an uninstalled app works on: what the
// record says was provisioned, with what the profile declares added to it.
// The record wins on the engine, because it says what was made and the
// profile only what would be made now.
func MergeStores(declared Stores, recorded Provisioned) Stores {
	out := declared
	if recorded.DatabaseEngine != "" {
		out.Database = recorded.DatabaseEngine
	}
	out.S3 = out.S3 || recorded.Bucket != ""
	out.Redis = out.Redis || recorded.CacheUser != ""
	return out
}

// NewProvisionedRecord is an empty record for a tenant.
func NewProvisionedRecord(tenantName string) *corev1.ConfigMap {
	key := ProvisionedRecordKey(tenantName)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				meta.TenantLabel:    tenantName,
				meta.ManagedByLabel: meta.ManagedByValue,
				ConfigTypeLabel:     ProvisionedConfigType,
			},
		},
	}
}

// ReadProvisioned is what a record says, by app. A nil record says nothing.
// An entry that does not parse is an error: a record that cannot be read is
// not a record of nothing.
func ReadProvisioned(record *corev1.ConfigMap) (map[string]Provisioned, error) {
	out := map[string]Provisioned{}
	if record == nil {
		return out, nil
	}
	for app, raw := range record.Data {
		var p Provisioned
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, fmt.Errorf("the record %s/%s has an entry for %q that does not parse: %w",
				record.Namespace, record.Name, app, err)
		}
		out[app] = p
	}
	return out, nil
}

// RecordProvisioned adds what was provisioned for an app to the record and
// reports whether the record changed. It only adds.
func RecordProvisioned(record *corev1.ConfigMap, app string, made Provisioned) (bool, error) {
	if made.Empty() {
		return false, nil
	}
	have, err := entryOf(record, app)
	if err != nil {
		return false, err
	}
	return setEntry(record, app, have.merged(made))
}

// ForgetProvisioned takes one kind out of an app's entry -- its store has
// been destroyed -- and the entry with it once nothing is left. It reports
// whether the record changed.
func ForgetProvisioned(record *corev1.ConfigMap, app string, kind Kind) (bool, error) {
	if record == nil {
		return false, nil
	}
	if _, ok := record.Data[app]; !ok {
		return false, nil
	}
	have, err := entryOf(record, app)
	if err != nil {
		return false, err
	}
	return setEntry(record, app, have.Without(kind))
}

func entryOf(record *corev1.ConfigMap, app string) (Provisioned, error) {
	var p Provisioned
	raw, ok := record.Data[app]
	if !ok {
		return p, nil
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("the record %s/%s has an entry for %q that does not parse: %w",
			record.Namespace, record.Name, app, err)
	}
	return p, nil
}

func setEntry(record *corev1.ConfigMap, app string, p Provisioned) (bool, error) {
	if p.Empty() {
		if _, ok := record.Data[app]; !ok {
			return false, nil
		}
		delete(record.Data, app)
		return true, nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	if record.Data[app] == string(raw) {
		return false, nil
	}
	if record.Data == nil {
		record.Data = map[string]string{}
	}
	record.Data[app] = string(raw)
	return true, nil
}

// ProvisionedApps are the apps a record names a store of the kind for, in
// order.
func ProvisionedApps(recorded map[string]Provisioned, has func(Provisioned) bool) []string {
	var apps []string
	for app, p := range recorded {
		if has(p) {
			apps = append(apps, app)
		}
	}
	sort.Strings(apps)
	return apps
}
