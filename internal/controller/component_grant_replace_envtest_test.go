/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What the operator does with an approval given again for other terms, as
// the API server takes it: rewritten in place it is refused, and removed in
// one write and added in the next it is accepted.
func TestAReplacedApprovalIsRemovedThenAddedAsTheAPIRequires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "grant-replace"}}
	if err := testClient.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), ns) })

	first := metav1.NewTime(time.Date(2026, 11, 6, 0, 56, 48, 0, time.UTC))
	again := metav1.NewTime(time.Date(2026, 11, 6, 9, 46, 15, 0, time.UTC))
	grant := func(at metav1.Time, digest string) gentianov1alpha1.PrivilegeGrant {
		return gentianov1alpha1.PrivilegeGrant{Privilege: "apiExtensions/schedules", Approver: "u-owner",
			ApprovedAt: at, Reason: "the scheduled backups", Digest: "sha256:" + strings.Repeat(digest, 64)}
	}
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: ns.Name},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: "scheduler"},
			Class:      gentianov1alpha1.ComponentClassApp,
			Privileges: []gentianov1alpha1.PrivilegeGrant{grant(first, "d")},
		},
	}
	if err := testClient.Create(ctx, comp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), comp) })

	inPlace := comp.DeepCopy()
	inPlace.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{grant(again, "4")}
	if err := testClient.Update(ctx, inPlace); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("an approval rewritten in place was not refused: %v", err)
	}

	comp.Spec.Privileges = nil
	if err := testClient.Update(ctx, comp); err != nil {
		t.Fatalf("removing the approval: %v", err)
	}
	comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{grant(again, "4")}
	if err := testClient.Update(ctx, comp); err != nil {
		t.Fatalf("adding the new approval: %v", err)
	}
}
