/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usagestatistics

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const newUID = "11111111-2222-4333-8444-555555555555"

// running starts the reporter's loop with the daily report an hour away and
// the tenants checked often, and stops it at the end of the test.
func running(t *testing.T, r *Reporter, minGap time.Duration) {
	t.Helper()
	r.InitialDelay, r.Interval = time.Millisecond, time.Hour
	r.CheckEvery, r.MinGap = time.Millisecond, minGap
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("the reporter returned an error to the manager: %v", err)
		}
	})
}

func waitFor(t *testing.T, e *endpoint, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.received() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.received() < n {
		t.Fatalf("%d reports arrived, want %d", e.received(), n)
	}
}

func names(t *testing.T, e *endpoint, i int) map[string]bool {
	t.Helper()
	e.mu.Lock()
	body := e.bodies[i]
	e.mu.Unlock()
	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, tenant := range report.Tenants {
		out[tenant.UID] = true
	}
	return out
}

func provision(t *testing.T, r *Reporter) {
	t.Helper()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "newco", UID: newUID},
		Status:     gentianov1alpha1.TenantStatus{Domain: "newco.example"},
	}
	if err := r.Client.Create(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
}

// A tenant provisioned after the day's report is named in a report that goes
// soon after, not the next day: the receiving end finds a tenant by what the
// latest report names. Once it has gone, nothing more is sent while the
// tenants stay as they are.
func TestANewTenantIsReportedWithoutWaitingForTheDay(t *testing.T) {
	e := newEndpoint(t, http.StatusAccepted)
	r := reporter(t, e, newClient(t, tenants()...))
	running(t, r, time.Millisecond)

	waitFor(t, e, 1)
	if names(t, e, 0)[newUID] {
		t.Fatal("the first report names a tenant that does not exist yet")
	}
	time.Sleep(50 * time.Millisecond)
	if e.received() != 1 {
		t.Fatalf("%d reports went while the tenants stayed the same", e.received())
	}

	provision(t, r)
	waitFor(t, e, 2)
	if !names(t, e, 1)[newUID] {
		t.Fatal("the report after the change does not name the new tenant")
	}
	time.Sleep(50 * time.Millisecond)
	if e.received() != 2 {
		t.Fatalf("%d reports went after the change was reported", e.received())
	}
}

// A change does not send a report sooner than the least gap after the last
// one: tenants provisioned in a burst go in a few reports, not one each.
func TestAChangeWaitsOutTheGapAfterTheLastReport(t *testing.T) {
	e := newEndpoint(t, http.StatusAccepted)
	r := reporter(t, e, newClient(t, tenants()...))
	running(t, r, time.Hour)

	waitFor(t, e, 1)
	provision(t, r)
	time.Sleep(100 * time.Millisecond)
	if e.received() != 1 {
		t.Fatalf("%d reports went inside the gap", e.received())
	}
}

// Names is what a report names: the address and realm of every tenant that
// has a host and is not being deleted.
func TestNamesAreTheTenantsAReportNames(t *testing.T) {
	c := newClient(t, tenants()...)
	id := Identity{ClusterID: "demo-cluster", KernelDomain: "k.example", TenancyMode: "multi"}
	got, err := Names(context.Background(), c, id)
	if err != nil {
		t.Fatal(err)
	}
	built, err := Build(context.Background(), c, counter(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(built) {
		t.Fatalf("Names has %d tenants, a report %d", len(got), len(built))
	}
	for _, tenant := range built {
		if got[tenant.UID] != tenant.URL+" "+tenant.Issuer {
			t.Errorf("Names[%q] = %q, the report says %q %q", tenant.UID, got[tenant.UID], tenant.URL, tenant.Issuer)
		}
	}
}
