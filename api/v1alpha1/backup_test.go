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

package v1alpha1

import (
	"reflect"
	"testing"
)

// A profile that declares no backup block must still be captured correctly.
// Most of the catalogue will never set spec.backup, so these defaults are the
// contract for nearly every app, not an edge case.
func TestBackupDefaultsForProfileDeclaringNothing(t *testing.T) {
	var spec *BackupSpec // nil: exactly what a profile without the block yields

	if got := spec.QuiesceMode(); got != BackupQuiesceScaleDown {
		t.Errorf("QuiesceMode() = %q, want %q", got, BackupQuiesceScaleDown)
	}
	if got := spec.ConsistencyMode(); got != BackupConsistencyApp {
		t.Errorf("ConsistencyMode() = %q, want %q", got, BackupConsistencyApp)
	}
	if got := spec.IncludedVolumes(); got != nil {
		t.Errorf("IncludedVolumes() = %v, want nil (every release-owned claim)", got)
	}
	if got := spec.ExcludedPaths(); got != nil {
		t.Errorf("ExcludedPaths() = %v, want nil", got)
	}
	if got := spec.BoundSecretRefs(); got != nil {
		t.Errorf("BoundSecretRefs() = %v, want nil", got)
	}
	if pre, post := spec.QuiesceCommands(); pre != nil || post != nil {
		t.Errorf("QuiesceCommands() = (%v, %v), want (nil, nil)", pre, post)
	}
	if post, verify := spec.RestoreCommands(); post != nil || verify != nil {
		t.Errorf("RestoreCommands() = (%v, %v), want (nil, nil)", post, verify)
	}
	if got := spec.QuiesceContainer(); got != "" {
		t.Errorf("QuiesceContainer() = %q, want \"\" (pod's first container)", got)
	}
}

// An empty block is not a way to opt out of quiescing: it must resolve to the
// same defaults as declaring nothing at all.
func TestBackupEmptyBlockMatchesNilDefaults(t *testing.T) {
	spec := &BackupSpec{}

	if got := spec.QuiesceMode(); got != BackupQuiesceScaleDown {
		t.Errorf("QuiesceMode() = %q, want %q", got, BackupQuiesceScaleDown)
	}
	if got := spec.ConsistencyMode(); got != BackupConsistencyApp {
		t.Errorf("ConsistencyMode() = %q, want %q", got, BackupConsistencyApp)
	}

	withEmptyQuiesce := &BackupSpec{Quiesce: &BackupQuiesce{}}
	if got := withEmptyQuiesce.QuiesceMode(); got != BackupQuiesceScaleDown {
		t.Errorf("QuiesceMode() with empty quiesce = %q, want %q", got, BackupQuiesceScaleDown)
	}
}

func TestBackupDeclaredValuesWin(t *testing.T) {
	spec := &BackupSpec{
		Quiesce: &BackupQuiesce{
			Mode:      BackupQuiesceCommand,
			Pre:       []string{"occ", "maintenance:mode", "--on"},
			Post:      []string{"occ", "maintenance:mode", "--off"},
			Container: "nextcloud",
		},
		Volumes: &BackupVolumes{
			Include:      []string{"nextcloud-data"},
			ExcludePaths: []string{"appdata_*/preview"},
		},
		Restore: &BackupRestore{
			Post:   [][]string{{"occ", "maintenance:data-fingerprint"}},
			Verify: []string{"occ", "status"},
		},
		Consistency: BackupConsistencyPerStore,
	}

	if got := spec.QuiesceMode(); got != BackupQuiesceCommand {
		t.Errorf("QuiesceMode() = %q, want %q", got, BackupQuiesceCommand)
	}
	if got := spec.ConsistencyMode(); got != BackupConsistencyPerStore {
		t.Errorf("ConsistencyMode() = %q, want %q", got, BackupConsistencyPerStore)
	}
	if got := spec.QuiesceContainer(); got != "nextcloud" {
		t.Errorf("QuiesceContainer() = %q, want %q", got, "nextcloud")
	}

	pre, post := spec.QuiesceCommands()
	if !reflect.DeepEqual(pre, []string{"occ", "maintenance:mode", "--on"}) {
		t.Errorf("QuiesceCommands() pre = %v", pre)
	}
	if !reflect.DeepEqual(post, []string{"occ", "maintenance:mode", "--off"}) {
		t.Errorf("QuiesceCommands() post = %v", post)
	}

	if got := spec.IncludedVolumes(); !reflect.DeepEqual(got, []string{"nextcloud-data"}) {
		t.Errorf("IncludedVolumes() = %v", got)
	}
	if got := spec.ExcludedPaths(); !reflect.DeepEqual(got, []string{"appdata_*/preview"}) {
		t.Errorf("ExcludedPaths() = %v", got)
	}

	restorePost, verify := spec.RestoreCommands()
	if !reflect.DeepEqual(restorePost, [][]string{{"occ", "maintenance:data-fingerprint"}}) {
		t.Errorf("RestoreCommands() post = %v", restorePost)
	}
	if !reflect.DeepEqual(verify, []string{"occ", "status"}) {
		t.Errorf("RestoreCommands() verify = %v", verify)
	}
}

// Quiesce commands belong to command mode alone. Returning them under
// scaleDown would run an app's maintenance hooks against workloads that are
// being scaled to zero anyway.
func TestBackupQuiesceCommandsOnlyInCommandMode(t *testing.T) {
	spec := &BackupSpec{
		Quiesce: &BackupQuiesce{
			Mode: BackupQuiesceScaleDown,
			Pre:  []string{"occ", "maintenance:mode", "--on"},
			Post: []string{"occ", "maintenance:mode", "--off"},
		},
	}

	if pre, post := spec.QuiesceCommands(); pre != nil || post != nil {
		t.Errorf("QuiesceCommands() under scaleDown = (%v, %v), want (nil, nil)", pre, post)
	}
}
