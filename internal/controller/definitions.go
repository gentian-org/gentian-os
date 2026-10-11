/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// generatedKindsWritten is the Crossplane kinds each reconciler writes.
//
// Every reconciler of one of the operator's own kinds is held while any
// definition the chart delivers is older than the operator; that needs
// nothing said here (crdcheck.Holder.Guard). These two also write a kind
// Crossplane generates from an XRD, which reaches a cluster by installer step
// B-06 and not with the chart, so each names it. They are the only two: the
// operator reads Repository and Cluster claims and writes neither.
//
// This is the one list in the definitions check that is kept by hand, because
// which reconciler writes a kind is not written down anywhere a program can
// read. What a program can read is which of these kinds the operator is
// PERMITTED to write, and definitions_test.go fails when that and this list
// disagree.
var generatedKindsWritten = map[string][]string{
	// The XTenant the tenant Composition renders from: a quota or a security
	// setting its definition does not know never reaches the Composition.
	"tenant": {xTenantGVK.Kind},
	// The App claim: its pull secrets and addons are lost to a definition
	// that does not know them.
	"component": {appClaimGVK.Kind},
}

// guarded is the tenant reconciler behind the definitions check.
func (r *TenantReconciler) guarded() reconcile.Reconciler {
	return r.Definitions.Guard(r.Client, &gentianov1alpha1.Tenant{}, r, generatedKindsWritten["tenant"]...)
}

// guarded is the component reconciler behind the definitions check.
func (r *ComponentReconciler) guarded() reconcile.Reconciler {
	return r.Definitions.Guard(r.Client, &gentianov1alpha1.Component{}, r, generatedKindsWritten["component"]...)
}
