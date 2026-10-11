/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"reflect"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Provisioning only ever adds to the record, a purge takes out the kind it
// destroyed, and an entry is gone when nothing is left of it.
func TestTheRecordOfWhatWasProvisioned(t *testing.T) {
	record := NewProvisionedRecord("demo")
	if record.Namespace == "" || record.Name != "tenant-demo-provisioned-stores" {
		t.Fatalf("record = %s/%s", record.Namespace, record.Name)
	}
	if record.Labels["gentianos.io/tenant"] != "demo" {
		t.Errorf("labels = %v", record.Labels)
	}

	full := Provisioned{
		DatabaseEngine: gentianov1alpha1.DatabaseEngineMariaDB, Database: "demo_wiki", DatabaseUser: "demo_wiki",
		Bucket: "demo-wiki", CacheUser: "demo-wiki",
	}
	if changed, err := RecordProvisioned(record, "wiki", full); err != nil || !changed {
		t.Fatalf("first write: changed = %v, err = %v", changed, err)
	}
	if changed, _ := RecordProvisioned(record, "wiki", full); changed {
		t.Error("writing the same entry again changed the record")
	}
	// A profile that no longer declares a bucket has not removed the bucket.
	lesser := full.Without(KindObjectStorage)
	if changed, _ := RecordProvisioned(record, "wiki", lesser); changed {
		t.Error("provisioning took a store out of the record")
	}
	// Nothing to record writes nothing.
	if changed, _ := RecordProvisioned(record, "static-site", Provisioned{}); changed || len(record.Data) != 1 {
		t.Errorf("an app without stores got an entry: %v", record.Data)
	}

	got, err := ReadProvisioned(record)
	if err != nil || !reflect.DeepEqual(got, map[string]Provisioned{"wiki": full}) {
		t.Fatalf("read = %+v, %v", got, err)
	}
	if apps := ProvisionedApps(got, func(p Provisioned) bool { return p.Has(KindObjectStorage) }); !reflect.DeepEqual(apps, []string{"wiki"}) {
		t.Errorf("apps with a bucket = %v", apps)
	}

	for _, kind := range []Kind{KindObjectStorage, KindCache} {
		if changed, err := ForgetProvisioned(record, "wiki", kind); err != nil || !changed {
			t.Fatalf("forget %s: changed = %v, err = %v", kind, changed, err)
		}
	}
	got, _ = ReadProvisioned(record)
	if got["wiki"].Has(KindObjectStorage) || got["wiki"].Has(KindCache) || !got["wiki"].Has(KindDatabase) {
		t.Fatalf("after the bucket and the cache user were destroyed: %+v", got["wiki"])
	}
	if _, err := ForgetProvisioned(record, "wiki", KindDatabase); err != nil {
		t.Fatal(err)
	}
	if _, still := record.Data["wiki"]; still {
		t.Errorf("an entry with nothing left is still there: %v", record.Data)
	}
	// Forgetting what is not on record is nothing.
	if changed, err := ForgetProvisioned(record, "wiki", KindDatabase); err != nil || changed {
		t.Errorf("changed = %v, err = %v", changed, err)
	}
}

// A record that cannot be read is not a record of nothing.
func TestAnUnreadableRecordIsAnError(t *testing.T) {
	record := NewProvisionedRecord("demo")
	record.Data = map[string]string{"wiki": "{not json"}
	if _, err := ReadProvisioned(record); err == nil {
		t.Fatal("an entry that does not parse was read as nothing")
	}
	if _, err := RecordProvisioned(record, "wiki", Provisioned{Bucket: "demo-wiki"}); err == nil {
		t.Fatal("an entry that does not parse was overwritten")
	}
	if got, err := ReadProvisioned(nil); err != nil || len(got) != 0 {
		t.Fatalf("no record: %v, %v", got, err)
	}
}

// What a purge works on: the record says what was made, and wins on the
// engine; the profile adds what it declares.
func TestMergeStores(t *testing.T) {
	declared := Stores{Database: gentianov1alpha1.DatabaseEnginePostgreSQL, Redis: true}
	recorded := Provisioned{DatabaseEngine: gentianov1alpha1.DatabaseEngineMariaDB, Database: "d", Bucket: "b"}
	want := Stores{Database: gentianov1alpha1.DatabaseEngineMariaDB, S3: true, Redis: true}
	if got := MergeStores(declared, recorded); got != want {
		t.Errorf("merged = %+v, want %+v", got, want)
	}
	if got := MergeStores(declared, Provisioned{}); got != declared {
		t.Errorf("nothing recorded: %+v, want what the profile declares", got)
	}
}
