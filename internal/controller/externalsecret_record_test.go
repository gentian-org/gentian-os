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
	"fmt"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/credentialprobe"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// outcome reports what an ensure returned besides its error, in the
// transcript's err field.
func outcome(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

func externalSecretComponent() *gentianov1alpha1.Component {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "wiki", "tenant-demo", "uid-wiki"
	return comp
}

func externalSecretDrift(obj client.Object) {
	u := obj.(*unstructured.Unstructured)
	u.Object["spec"] = map[string]any{"refreshInterval": "5m"}
	u.SetLabels(map[string]string{"drifted": "true"})
	u.SetOwnerReferences(nil)
}

// What each ExternalSecret ensure writes, held to
// testdata/external-secrets.txt.
func TestExternalSecretTranscript(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	owner := []metav1.OwnerReference{{APIVersion: "gentianos.io/v1alpha1", Kind: "BundleSource", Name: "b", UID: "uid-b"}}
	cases := []ensureCase{
		{
			name: "backup probe",
			run: func(ctx context.Context, c client.Client) error {
				return applyExternalSecret(ctx, c, "credreq-x", meta.OperatorNamespace, "gentian-os/kernel/backup/x", "None", nil,
					map[string]string{"a": "b"})
			},
			written: unstructuredOf(secrets.ExternalSecretGVK, meta.OperatorNamespace, "credreq-x"),
			drift:   externalSecretDrift,
		},
		{
			name: "backup owned",
			run: func(ctx context.Context, c client.Client) error {
				return applyExternalSecret(ctx, c, "dest", "kernel-s3", "gentian-os/kernel/backup/x", "Owner", owner,
					map[string]string{"a": "b"})
			},
			written: unstructuredOf(secrets.ExternalSecretGVK, "kernel-s3", "dest"),
			drift:   externalSecretDrift,
		},
		{
			name: "sign-in secret",
			run: func(ctx context.Context, c client.Client) error {
				r := &ComponentReconciler{Client: c, Scheme: c.Scheme()}
				s := &signInSidecar{service: "wiki-sign-in", database: true, secrets: []string{"api"}}
				handed, err := r.ensureSignInSecret(ctx, externalSecretComponent(), tenant, s, map[string]string{"a": "b"})
				if err != nil {
					return err
				}
				return outcome("handed=%v", handed)
			},
			written: unstructuredOf(secrets.ExternalSecretGVK, "tenant-demo", "wiki-sign-in"),
			drift:   externalSecretDrift,
		},
		{
			name: "sign-in secret, nothing handed",
			seed: []client.Object{unstructuredOf(secrets.ExternalSecretGVK, "tenant-demo", "wiki-sign-in")},
			run: func(ctx context.Context, c client.Client) error {
				r := &ComponentReconciler{Client: c, Scheme: c.Scheme()}
				s := &signInSidecar{service: "wiki-sign-in"}
				handed, err := r.ensureSignInSecret(ctx, externalSecretComponent(), tenant, s, map[string]string{"a": "b"})
				if err != nil {
					return err
				}
				return outcome("handed=%v", handed)
			},
			written: unstructuredOf(secrets.ExternalSecretGVK, "tenant-demo", "wiki-sign-in"),
			drift:   externalSecretDrift,
		},
		{
			name: "authorization engine",
			run: func(ctx context.Context, c client.Client) error {
				r := &AuthorizationEngineReconciler{Client: c}
				return r.applyExternalSecret(ctx, "datastore", map[string]any{"uri": "{{ .user }}"},
					[]any{map[string]any{"secretKey": "user", "remoteRef": map[string]any{"key": "p", "property": "user"}}})
			},
			written: unstructuredOf(secrets.ExternalSecretGVK, arbiter.Namespace, "datastore"),
			drift:   externalSecretDrift,
		},
		{
			name: "desktop database",
			run: func(ctx context.Context, c client.Client) error {
				r := &ComponentReconciler{Client: c, Scheme: c.Scheme()}
				ready, reason, message, err := r.ensureDatabaseSecret(ctx, externalSecretComponent(), "demo", "openbao-tenant-demo",
					map[string]interface{}{"url": "{{ .password }}"},
					[]interface{}{map[string]interface{}{"secretKey": "password", "remoteRef": map[string]interface{}{"key": "p", "property": "password"}}})
				if err != nil {
					return err
				}
				return outcome("ready=%v reason=%q message=%q", ready, reason, message)
			},
			written: unstructuredOf(secrets.ExternalSecretGVK, "tenant-demo", "wiki"+componentDatabaseSecretSuffix),
			drift: func(obj client.Object) {
				externalSecretDrift(obj)
				u := obj.(*unstructured.Unstructured)
				u.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
			},
		},
	}
	holdToTranscript(t, *updateTranscripts, "external-secrets", ensureTranscript(t, cases))
}

// What the readers of a credential probe say of the same probes, held to
// testdata/credential-probes.txt.
func TestCredentialProbeTranscript(t *testing.T) {
	probes := map[string]map[string]any{
		"no status":          nil,
		"no conditions":      {"conditions": []any{}},
		"other condition":    {"conditions": []any{map[string]any{"type": "Synced", "status": "True"}}},
		"ready":              {"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": "2026-01-02T03:04:05Z"}}},
		"not ready, message": {"conditions": []any{map[string]any{"type": "Ready", "status": "False", "message": "the key is missing"}}},
		"not ready, silent":  {"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}},
		"unknown":            {"conditions": []any{map[string]any{"type": "Ready", "status": "Unknown", "message": "pending"}}},
		"ready second":       {"conditions": []any{map[string]any{"type": "Synced", "status": "False"}, map[string]any{"type": "Ready", "status": "True"}}},
		"malformed":          {"conditions": "x"},
	}
	var out []string
	ctx := context.Background()
	for name, status := range probes {
		var seed []client.Object
		es := unstructuredOf(secrets.ExternalSecretGVK, meta.OperatorNamespace, credentialprobe.Prefix+"req")
		if status != nil {
			es.Object["status"] = status
		}
		seed = append(seed, es)
		c := fake.NewClientBuilder().WithScheme(ensureScheme(t)).WithObjects(seed...).Build()
		ok, msg := credentialProbeSatisfied(ctx, c, "req")
		out = append(out, fmt.Sprintf("%s: satisfied=%v message=%q", name, ok, msg))
	}
	c := fake.NewClientBuilder().WithScheme(ensureScheme(t)).Build()
	ok, msg := credentialProbeSatisfied(ctx, c, "req")
	out = append(out, fmt.Sprintf("absent: satisfied=%v message=%q", ok, msg))
	sort.Strings(out)
	holdToTranscript(t, *updateTranscripts, "credential-probes", strings.Join(out, "\n")+"\n")
}
