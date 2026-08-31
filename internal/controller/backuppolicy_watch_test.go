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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func probe(name, namespace string) client.Object {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(externalSecretGVK)
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
