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
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// staleSeed is one object of each kind a stale delete looks at, in every
// combination of labels and namespace that decides whether it is looked at.
func staleSeed() []client.Object {
	var out []client.Object
	labelSets := map[string]map[string]string{
		"tenant":       {managedByLabel: managedByValue, tenantLabel: "demo", gatewayComponentLabel: gatewayComponentApp},
		"other-tenant": {managedByLabel: managedByValue, tenantLabel: "other", gatewayComponentLabel: gatewayComponentApp},
		"kernel":       {managedByLabel: managedByValue, gatewayComponentLabel: gatewayComponentKernel},
		"unmanaged":    {gatewayComponentLabel: gatewayComponentApp, tenantLabel: "demo"},
	}
	for setName, labels := range labelSets {
		for _, ns := range []string{"tenant-demo", servicesNamespace} {
			for _, keep := range []string{"keep", "drop"} {
				name := setName + "-" + keep
				route := &gatewayv1.HTTPRoute{}
				route.Name, route.Namespace, route.Labels = name, ns, labels
				out = append(out, route)
				for _, gvk := range []schema.GroupVersionKind{backendTrafficPolicyGVK, clientTrafficPolicyGVK, securityPolicyGVK} {
					u := unstructuredOf(gvk, ns, name)
					u.SetLabels(labels)
					out = append(out, u)
				}
			}
		}
	}
	return out
}

// What the stale deletes remove, held to testdata/stale-deletes.txt.
func TestStaleDeleteTranscript(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	keep := map[string]struct{}{"tenant-keep": {}, "kernel-keep": {}, "other-tenant-keep": {}, "unmanaged-keep": {}}
	type run func(ctx context.Context, c client.Client) error
	runs := map[string]run{
		"tenant routes": func(ctx context.Context, c client.Client) error {
			return (&TenantReconciler{Client: c}).deleteStaleHTTPRoutesForTenant(ctx, tenant, "tenant-demo", keep)
		},
		"tenant routes, all": func(ctx context.Context, c client.Client) error {
			return (&TenantReconciler{Client: c}).deleteStaleHTTPRoutesForTenant(ctx, tenant, "tenant-demo", nil)
		},
		"tenant backend policies": func(ctx context.Context, c client.Client) error {
			return (&TenantReconciler{Client: c}).deleteStaleBackendTrafficPoliciesForTenant(ctx, tenant, "tenant-demo", keep)
		},
		"tenant backend policies, all": func(ctx context.Context, c client.Client) error {
			return (&TenantReconciler{Client: c}).deleteStaleBackendTrafficPoliciesForTenant(ctx, tenant, "tenant-demo", nil)
		},
		"tenant client policies": func(ctx context.Context, c client.Client) error {
			return (&TenantReconciler{Client: c}).deleteStaleClientTrafficPoliciesForTenant(ctx, tenant, "tenant-demo", keep)
		},
		"tenant client policies, all": func(ctx context.Context, c client.Client) error {
			return (&TenantReconciler{Client: c}).deleteStaleClientTrafficPoliciesForTenant(ctx, tenant, "tenant-demo", nil)
		},
		"kernel routes and policies": func(ctx context.Context, c client.Client) error {
			return (&GatewayPlatformReconciler{Client: c}).deleteStaleKernelRoutesAndPolicies(ctx, keep, keep)
		},
		"kernel routes and policies, none listed": func(ctx context.Context, c client.Client) error {
			return (&GatewayPlatformReconciler{Client: c}).deleteStaleKernelRoutesAndPolicies(ctx, nil, map[string]struct{}{})
		},
	}
	var out []string
	for name, do := range runs {
		for _, failure := range []string{"", "no match", "list fails", "delete fails", "delete not found"} {
			var deleted []string
			cl := fake.NewClientBuilder().WithScheme(ensureScheme(t)).WithObjects(staleSeed()...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					switch failure {
					case "no match":
						return &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "g", Kind: "K"}}
					case "list fails":
						return errors.New("list refused")
					}
					return cl.List(ctx, list, opts...)
				},
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					kind := fmt.Sprintf("%T", obj)
					if u, ok := obj.(*unstructured.Unstructured); ok {
						kind = u.GetKind()
					}
					deleted = append(deleted, fmt.Sprintf("%s %s/%s", kind, obj.GetNamespace(), obj.GetName()))
					switch failure {
					case "delete fails":
						return errors.New("delete refused")
					case "delete not found":
						return &notFoundError{}
					}
					return cl.Delete(ctx, obj, opts...)
				},
			}).Build()
			err := do(context.Background(), cl)
			sort.Strings(deleted)
			out = append(out, fmt.Sprintf("%s, %q: err=%v deleted=%q", name, failure, err, deleted))
		}
	}
	sort.Strings(out)
	holdToTranscript(t, *updateTranscripts, "stale-deletes", strings.Join(out, "\n")+"\n")
}

// notFoundError is the API server's answer for an object already gone.
type notFoundError struct{}

func (notFoundError) Error() string { return "not found" }
func (notFoundError) Status() metav1.Status {
	return metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: 404}
}

func keyProfile(services gentianov1alpha1.ServiceRequirements) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Name = "wiki"
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &services}
	return p
}

// What the two key Secrets are kept as, held to testdata/key-secrets.txt.
func TestKeySecretTranscript(t *testing.T) {
	comp := externalSecretlessComponent()
	foreign := &gentianov1alpha1.Component{}
	foreign.Name, foreign.Namespace, foreign.UID = "other", comp.Namespace, "uid-other"
	type ensure func(ctx context.Context, r *ComponentReconciler, profile *gentianov1alpha1.ComponentProfile) error
	ensures := map[string]struct {
		run     ensure
		name    string
		key     string
		profile map[string]*gentianov1alpha1.ComponentProfile
	}{
		"authorization": {
			run: func(ctx context.Context, r *ComponentReconciler, p *gentianov1alpha1.ComponentProfile) error {
				return r.ensureAuthorization(ctx, comp, "demo", p)
			},
			name: authorizationSecretName(comp.Name), key: authorizationKeyKey,
			profile: map[string]*gentianov1alpha1.ComponentProfile{
				"declared":     keyProfile(gentianov1alpha1.ServiceRequirements{Authorization: &gentianov1alpha1.AuthorizationRequirement{}}),
				"not declared": keyProfile(gentianov1alpha1.ServiceRequirements{}),
			},
		},
		"rights check": {
			run: func(ctx context.Context, r *ComponentReconciler, p *gentianov1alpha1.ComponentProfile) error {
				return r.ensureRightsCheck(ctx, comp, "demo", p)
			},
			name: rightsCheckSecretName(comp.Name), key: rightsCheckKeyKey,
			profile: map[string]*gentianov1alpha1.ComponentProfile{
				"rights":       keyProfile(gentianov1alpha1.ServiceRequirements{Rights: &gentianov1alpha1.RightsRequirement{}}),
				"vouching":     keyProfile(gentianov1alpha1.ServiceRequirements{Vouching: &gentianov1alpha1.VouchingRequirement{}}),
				"commission":   keyProfile(gentianov1alpha1.ServiceRequirements{Partners: &gentianov1alpha1.PartnersRequirement{Commission: true}}),
				"not declared": keyProfile(gentianov1alpha1.ServiceRequirements{}),
			},
		},
	}
	drifts := map[string]func(s *corev1.Secret){
		"none":          func(*corev1.Secret) {},
		"no key":        func(s *corev1.Secret) { delete(s.Data, authorizationKeyKey); delete(s.Data, rightsCheckKeyKey) },
		"other url":     func(s *corev1.Secret) { s.Data[authorizationURLKey] = []byte("x"); s.Data[rightsCheckURLKey] = []byte("x") },
		"director":      func(s *corev1.Secret) { s.Data[componentDirectorURLKey] = []byte("http://elsewhere") },
		"no label":      func(s *corev1.Secret) { s.Labels = map[string]string{"a": "b"} },
		"extra data":    func(s *corev1.Secret) { s.Data["extra"] = []byte("y") },
		"not its owner": func(s *corev1.Secret) { s.OwnerReferences = nil },
	}
	var out []string
	ctx := context.Background()
	for ename, e := range ensures {
		for pname, profile := range e.profile {
			for dname, drift := range drifts {
				var calls []string
				cl := fake.NewClientBuilder().WithScheme(ensureScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						calls = append(calls, "create")
						return cl.Create(ctx, obj, opts...)
					},
					Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						calls = append(calls, "patch")
						return cl.Patch(ctx, obj, patch, opts...)
					},
				}).Build()
				r := &ComponentReconciler{Client: cl, Scheme: cl.Scheme()}
				first := r.describeKeySecret(t, e.run(ctx, r, profile), e.name, e.key, "")
				secret := &corev1.Secret{}
				key := client.ObjectKey{Namespace: comp.Namespace, Name: e.name}
				was := ""
				if cl.Get(ctx, key, secret) == nil {
					was = string(secret.Data[e.key])
					drift(secret)
					if err := cl.Update(ctx, secret); err != nil {
						t.Fatal(err)
					}
				}
				calls = nil
				again := r.describeKeySecret(t, e.run(ctx, r, profile), e.name, e.key, was)
				out = append(out, fmt.Sprintf("%s, %s, drift %s: first %s; after drift calls=%q %s", ename, pname, dname, first, calls, again))
			}
		}
	}
	sort.Strings(out)
	holdToTranscript(t, *updateTranscripts, "key-secrets", strings.Join(out, "\n")+"\n")
}

func externalSecretlessComponent() *gentianov1alpha1.Component {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "wiki", "tenant-demo", "uid-wiki"
	return comp
}

// describeKeySecret is the Secret as the transcript holds it: its key only
// as whether it is there and whether it is the one it was.
func (r *ComponentReconciler) describeKeySecret(t *testing.T, err error, name, keyField, was string) string {
	t.Helper()
	secret := &corev1.Secret{}
	if getErr := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-demo", Name: name}, secret); getErr != nil {
		return fmt.Sprintf("err=%v secret: %v", err, getErr)
	}
	var data []string
	for k, v := range secret.Data {
		switch {
		case k != keyField:
			data = append(data, k+"="+string(v))
		case len(v) == 0:
			data = append(data, k+"=<empty>")
		case was != "" && string(v) == was:
			data = append(data, k+"=<the same key>")
		default:
			data = append(data, fmt.Sprintf("%s=<a key of %d characters>", k, len(v)))
		}
	}
	sort.Strings(data)
	var owners []string
	for _, o := range secret.OwnerReferences {
		owners = append(owners, string(o.UID))
	}
	return fmt.Sprintf("err=%v labels=%v owners=%v data=%v", err, secret.Labels, owners, data)
}
