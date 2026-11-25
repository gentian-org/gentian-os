/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func probe(name, namespace string) client.Object {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(secrets.ExternalSecretGVK)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

func policyWaitingOn(name, requirement string) *gentianov1alpha1.BackupPolicy {
	return &gentianov1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     gentianov1alpha1.BackupPolicyStatus{CredentialRequirement: requirement},
	}
}

// The predicate is the difference between waking on the handful of probes this
// controller creates and waking on every app's ExternalSecret in the cluster,
// of which there is one per app per tenant.
func TestOnlyTheOperatorsOwnProbesReachTheWatch(t *testing.T) {
	cases := []struct {
		name, namespace string
		want            bool
	}{
		{"credreq-backup-destination-corp", meta.OperatorNamespace, true},
		{"credreq-backup-destination", meta.OperatorNamespace, true},
		{"backup-destination-corp", meta.OperatorNamespace, false},
		{"credreq-backup-destination-corp", "tenant-corp", false},
		{"nextcloud-base-ce-secrets", "tenant-corp", false},
	}
	for _, tc := range cases {
		if got := isCredentialProbe(probe(tc.name, tc.namespace)); got != tc.want {
			t.Errorf("isCredentialProbe(%s/%s) = %v, want %v",
				tc.namespace, tc.name, got, tc.want)
		}
	}
}

// The bug this watch closes: a policy reports CredentialUnsatisfied and returns
// without requeueing, so nothing tells it when the keys arrive. On corp the
// credential landed, ESO synced it, and the policy still read "supply
// backup-destination-corp" until someone annotated it.
func TestAProbeWakesThePolicyWaitingOnIt(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	corp := policyWaitingOn("corp", "backup-destination-corp")
	other := policyWaitingOn("acme", "backup-destination-acme")
	cluster := policyWaitingOn("default", "backup-destination")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(corp, other, cluster).Build()
	r := &BackupPolicyReconciler{Client: c, Scheme: s}

	reqs := r.policiesForProbe(context.Background(),
		probe("credreq-backup-destination-corp", meta.OperatorNamespace))

	var names []string
	for _, req := range reqs {
		names = append(names, req.Name)
	}
	if len(names) != 1 || names[0] != "corp" {
		t.Fatalf("enqueued %v, want just [corp] — a probe must wake the policy "+
			"waiting on it and leave the others alone", names)
	}
}

// A policy that has not reconciled yet has no requirement recorded. Including
// it costs one no-op reconcile and covers the ordering where the probe reports
// before the policy first writes down what it is waiting for — which is the
// ordering on a fresh cluster, where nothing would otherwise arrive.
func TestAPolicyWithNoRequirementRecordedIsStillWoken(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	fresh := policyWaitingOn("corp", "")
	unrelated := policyWaitingOn("acme", "backup-destination-acme")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(fresh, unrelated).Build()
	r := &BackupPolicyReconciler{Client: c, Scheme: s}

	reqs := r.policiesForProbe(context.Background(),
		probe("credreq-backup-destination-corp", meta.OperatorNamespace))

	if len(reqs) != 1 || reqs[0].Name != "corp" {
		t.Fatalf("enqueued %v, want the policy that has not recorded a "+
			"requirement yet", reqs)
	}
}

// The hot loop this closes: applyExternalSecret rewrote the object on every
// pass whether or not anything had changed. ESO writes it too, so the two
// raced; the loser got a conflict, the conflict was reported as a failed
// policy, and writing that status woke the next reconcile — which conflicted
// again. On corp the Accepted condition flipped between True/Accepted and
// False/CredentialUnavailable about once a second, indefinitely.
func TestSteadyStateWritesNothing(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}
	r := &BackupPolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).Build(),
		Scheme: s,
	}
	ctx := context.Background()
	const vault = "gentian-os/tenants/corp/backup/destination"

	if err := applyExternalSecret(ctx, r.Client, "backup-destination-corp", s3Namespace,
		vault, "Owner", nil, map[string]string{managedByLabel: managedByValue}); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := externalSecretVersion(t, r, "backup-destination-corp")

	// A second pass with identical inputs is what a steady-state reconcile is.
	if err := applyExternalSecret(ctx, r.Client, "backup-destination-corp", s3Namespace,
		vault, "Owner", nil, map[string]string{managedByLabel: managedByValue}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second := externalSecretVersion(t, r, "backup-destination-corp"); second != first {
		t.Errorf("an unchanged reconcile rewrote the ExternalSecret (%s -> %s); "+
			"that write is what ESO raced and the race was reported as a broken policy",
			first, second)
	}

	// A real change must still be written, or the object would never converge.
	if err := applyExternalSecret(ctx, r.Client, "backup-destination-corp", s3Namespace,
		"gentian-os/tenants/corp/backup/moved", "Owner", nil,
		map[string]string{managedByLabel: managedByValue}); err != nil {
		t.Fatalf("changed pass: %v", err)
	}
	if changed := externalSecretVersion(t, r, "backup-destination-corp"); changed == first {
		t.Error("a changed vault path was not written; the ExternalSecret would never converge")
	}
}

// The loop of gentian-os#1344: the API server fills in what the operator
// leaves out of an ExternalSecret, so the stored object never equalled the
// desired one and every pass replaced it. The replacement dropped ESO's
// finalizer; ESO put it back, which woke the next pass, about once a second.
// Here the stored object carries what the API server and ESO add, as on a
// cluster: a pass with unchanged inputs must not write, and a real change
// must keep ESO's finalizer and the retry's annotation.
func TestServerDefaultsAndFinalizerWriteNothing(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	r := &BackupPolicyReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	ctx := context.Background()
	const name, vault = "bundle-source-corp-backups", "gentian-os/kernel/backup/sources/corp-backups"
	labels := map[string]string{managedByLabel: managedByValue}
	pass := func(path string) {
		t.Helper()
		if err := applyExternalSecret(ctx, r.Client, name, s3Namespace, path, "Owner", nil, labels); err != nil {
			t.Fatalf("pass: %v", err)
		}
	}
	pass(vault)

	// What the API server and ESO add to the stored object.
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: s3Namespace}, es); err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = unstructured.SetNestedField(es.Object, "1h", "spec", "refreshInterval")
	_ = unstructured.SetNestedField(es.Object, "Retain", "spec", "target", "deletionPolicy")
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	for _, d := range data {
		ref := d.(map[string]any)["remoteRef"].(map[string]any)
		ref["conversionStrategy"], ref["decodingStrategy"], ref["metadataPolicy"] = "Default", "None", "None"
	}
	_ = unstructured.SetNestedSlice(es.Object, data, "spec", "data")
	es.SetFinalizers([]string{"externalsecrets.external-secrets.io/externalsecret-cleanup"})
	es.SetAnnotations(map[string]string{syncRetriedAnnotation: "1764000000"})
	if err := r.Update(ctx, es); err != nil {
		t.Fatalf("add defaults: %v", err)
	}
	stored := externalSecretVersion(t, r, name)

	pass(vault)
	if got := externalSecretVersion(t, r, name); got != stored {
		t.Fatalf("an unchanged pass rewrote an ExternalSecret the API server had defaulted (%s -> %s)", stored, got)
	}

	pass("gentian-os/kernel/backup/sources/moved")
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: s3Namespace}, es); err != nil {
		t.Fatalf("get: %v", err)
	}
	if f := es.GetFinalizers(); len(f) != 1 {
		t.Errorf("a real change dropped ESO's finalizer: %v", f)
	}
	if es.GetAnnotations()[syncRetriedAnnotation] == "" {
		t.Errorf("a real change dropped the retry's annotation: %v", es.GetAnnotations())
	}
	data, _, _ = unstructured.NestedSlice(es.Object, "spec", "data")
	if ref := data[0].(map[string]any)["remoteRef"].(map[string]any); ref["key"] != "gentian-os/kernel/backup/sources/moved" {
		t.Errorf("the changed vault path was not written: %v", ref["key"])
	}
}

func externalSecretVersion(t *testing.T, r *BackupPolicyReconciler, name string) string {
	t.Helper()
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	if err := r.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: s3Namespace}, es); err != nil {
		t.Fatalf("get ExternalSecret: %v", err)
	}
	return es.GetResourceVersion()
}
