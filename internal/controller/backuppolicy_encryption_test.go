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

package controller

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// The managed schedule carries the tenant's key, and "no key" is exactly one
// value.
//
// nil rather than an empty Recipients list matters more than it looks: the
// managed schedule is only rewritten when it differs from what the policy
// wants, compared with DeepEqual. Two spellings of "inherit" would differ for
// ever, so every reconcile would write the schedule again — and a
// TenantExportSchedule rewritten on a loop is a schedule whose next run time
// keeps moving.
func TestScheduleEncryptionDistinguishesInheritFromAKey(t *testing.T) {
	const tenantKey = "age17lr9cmnutfg66r92rwc20umdz82sgx3wq86c5lmht8d7sm8dlqpqr3d4zw"

	if got := scheduleEncryption(backup.Effective{}); got != nil {
		t.Errorf("encryption = %+v, want nil so the cluster's key applies", got)
	}
	if got := scheduleEncryption(backup.Effective{Recipients: []string{}}); got != nil {
		t.Errorf("an empty recipient list produced %+v, want the same nil as unset", got)
	}

	got := scheduleEncryption(backup.Effective{Recipients: []string{tenantKey}})
	if got == nil {
		t.Fatal("a stated recipient produced no encryption block")
	}
	// Stated rather than left to default: a schedule is read by people
	// deciding whether their backups are readable by the platform, and an
	// absent mode makes them look that answer up in the CRD.
	if got.Mode != gentianov1alpha1.ExportEncryptionRecipient {
		t.Errorf("mode = %q, want %q", got.Mode, gentianov1alpha1.ExportEncryptionRecipient)
	}
	if len(got.Recipients) != 1 || got.Recipients[0] != tenantKey {
		t.Errorf("recipients = %v, want the tenant's key", got.Recipients)
	}
}
