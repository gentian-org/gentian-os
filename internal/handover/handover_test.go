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

package handover

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const ns = "gentian-system"

var (
	first  = time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	second = time.Date(2026, 8, 19, 17, 30, 0, 0, time.UTC)
)

func newClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// No record is a state, not a failure: it is what every cluster looks like
// before anyone signs in, and a caller that could not tell the two apart would
// gate on infrastructure trouble.
func TestReadMissingIsNotAnError(t *testing.T) {
	state, err := Read(context.Background(), newClient(), ns)
	if err != nil {
		t.Fatalf("expected a missing record to read cleanly, got: %v", err)
	}
	if state.WritePathProven {
		t.Fatal("a cluster with no record has proven nothing")
	}
}

func TestRecordCreatesAndReadsBack(t *testing.T) {
	c := newClient()
	if err := RecordWritePathProven(context.Background(), c, ns, "admin@example.org", first); err != nil {
		t.Fatalf("record: %v", err)
	}
	state, err := Read(context.Background(), c, ns)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !state.WritePathProven {
		t.Fatal("expected the write path to read as proven")
	}
	if state.ProvenBy != "admin@example.org" {
		t.Errorf("provenBy = %q, want admin@example.org", state.ProvenBy)
	}
	if state.ProvenAt != first.Format(time.RFC3339) {
		t.Errorf("provenAt = %q, want %q", state.ProvenAt, first.Format(time.RFC3339))
	}
}

// The record answers "when was this FIRST seen to work" — the question someone
// deciding whether to revoke is asking. Refreshing it on every request would
// also mean an API write per API call.
func TestRecordDoesNotMoveTheFirstTimestamp(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if err := RecordWritePathProven(ctx, c, ns, "first@example.org", first); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if err := RecordWritePathProven(ctx, c, ns, "second@example.org", second); err != nil {
		t.Fatalf("second record: %v", err)
	}
	state, _ := Read(ctx, c, ns)
	if state.ProvenAt != first.Format(time.RFC3339) {
		t.Errorf("provenAt moved to %q; the first proof is the one that matters", state.ProvenAt)
	}
	if state.ProvenBy != "first@example.org" {
		t.Errorf("provenBy = %q, want the first prover", state.ProvenBy)
	}
}

// The revoke step patches the same object. Recording proof must not erase that.
func TestRecordPreservesTheRevocationFlag(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ns},
		Data:       map[string]string{KeyBootstrapRevoked: "true", KeyRevokedAt: "2026-08-18T10:00:00Z"},
	}
	c := newClient(existing)
	if err := RecordWritePathProven(context.Background(), c, ns, "admin", first); err != nil {
		t.Fatalf("record: %v", err)
	}
	state, _ := Read(context.Background(), c, ns)
	if !state.BootstrapRevoked {
		t.Error("recording proof erased the revocation flag")
	}
	if !state.WritePathProven {
		t.Error("expected proof to be recorded alongside the existing keys")
	}
}

// An exchange that came back with no user_claim still proves the role opened.
func TestRecordWithoutAUserStillProves(t *testing.T) {
	c := newClient()
	if err := RecordWritePathProven(context.Background(), c, ns, "", first); err != nil {
		t.Fatalf("record: %v", err)
	}
	state, _ := Read(context.Background(), c, ns)
	if !state.WritePathProven {
		t.Fatal("an anonymous-but-successful exchange is still proof")
	}
	if state.ProvenBy != "" {
		t.Errorf("provenBy = %q, want empty rather than invented", state.ProvenBy)
	}
}
