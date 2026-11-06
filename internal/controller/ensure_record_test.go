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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

// ensureScheme knows every kind an ensure below writes.
func ensureScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := SchemeForTest(t)
	for _, gvk := range []schema.GroupVersionKind{securityPolicyGVK, clientTrafficPolicyGVK, backendTrafficPolicyGVK, referenceGrantGVK, httpRouteFilterGVK, secrets.ExternalSecretGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

// ensureCase is one ensure: what it writes on an empty cluster, on the
// cluster it left, and after drift changed what it wrote.
type ensureCase struct {
	name string
	// seed is on the cluster before the first run.
	seed []client.Object
	run  func(ctx context.Context, c client.Client) error
	// written is the object the ensure keeps, read after each run.
	written client.Object
	// drift changes the written object on the cluster.
	drift func(obj client.Object)
}

// describeWritten is the object as the transcript holds it: its labels,
// annotations, owners and everything but its metadata.
func describeWritten(t *testing.T, obj client.Object) string {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	md, _ := m["metadata"].(map[string]any)
	m["metadata"] = map[string]any{"labels": md["labels"], "annotations": md["annotations"], "ownerReferences": md["ownerReferences"]}
	delete(m, "apiVersion")
	delete(m, "kind")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func ensureTranscript(t *testing.T, cases []ensureCase) string {
	t.Helper()
	var out []string
	for _, c := range cases {
		var calls []string
		record := func(verb string, obj client.Object) {
			calls = append(calls, fmt.Sprintf("%s %s/%s", verb, obj.GetNamespace(), obj.GetName()))
		}
		cl := fake.NewClientBuilder().WithScheme(ensureScheme(t)).WithObjects(c.seed...).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				record("create", obj)
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				record("update", obj)
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				data, _ := patch.Data(obj)
				record("patch "+string(patch.Type())+" "+string(data), obj)
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
		ctx := context.Background()
		step := func(name string) {
			t.Helper()
			calls = nil
			err := c.run(ctx, cl)
			got := c.written.DeepCopyObject().(client.Object)
			getErr := cl.Get(ctx, client.ObjectKeyFromObject(c.written), got)
			line := fmt.Sprintf("%s, %s: err=%v calls=%q", c.name, name, err, calls)
			if getErr != nil {
				line += " written: " + getErr.Error()
			} else {
				line += " written: " + describeWritten(t, got)
			}
			out = append(out, line)
		}
		step("first")
		step("again")
		obj := c.written.DeepCopyObject().(client.Object)
		if err := cl.Get(ctx, client.ObjectKeyFromObject(c.written), obj); err == nil {
			c.drift(obj)
			if err := cl.Update(ctx, obj); err != nil {
				t.Fatal(err)
			}
		}
		step("after drift")
	}
	sort.Strings(out)
	return strings.Join(out, "\n") + "\n"
}

func unstructuredOf(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func namedObject(obj client.Object, namespace, name string) client.Object {
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func testZone() edgeZone { return edgeZone{secretName: "zone-client"} }

func zoneSecretSource() *corev1.Secret {
	s := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"client-secret": []byte("s")}}
	s.Name, s.Namespace = testZone().secretName, servicesNamespace
	return s
}

func registrySource() *corev1.Secret {
	s := &corev1.Secret{Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{".dockerconfigjson": []byte("{}")}}
	s.Name, s.Namespace = "registry-credentials", servicesNamespace
	return s
}

func driftSpec(obj client.Object) {
	u := obj.(*unstructured.Unstructured)
	u.Object["spec"] = map[string]any{"drifted": true}
	u.SetLabels(map[string]string{"drifted": "true"})
}

// What each ensure writes, held to testdata/ensure.txt.
func TestEnsureTranscript(t *testing.T) {
	gp := &GatewayPlatformReconciler{}
	withClient := func(c client.Client) *GatewayPlatformReconciler { r := *gp; r.Client = c; return &r }
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	cases := []ensureCase{
		{
			name: "restore hold filter",
			run: func(ctx context.Context, c client.Client) error {
				return ensureRestoreHoldFilter(ctx, c, "tenant-demo")
			},
			written: unstructuredOf(httpRouteFilterGVK, "tenant-demo", restoreHoldFilterName),
			drift:   driftSpec,
		},
		{
			name: "route reference grant",
			run: func(ctx context.Context, c client.Client) error {
				return withClient(c).ensureRouteReferenceGrant(ctx, "kernel-auth")
			},
			written: unstructuredOf(referenceGrantGVK, "kernel-auth", "allow-kernel-gateway-routes"),
			drift:   driftSpec,
		},
		{
			name: "client traffic policy",
			run: func(ctx context.Context, c client.Client) error {
				return withClient(c).ensureKernelClientTrafficPolicyNamed(ctx, "ctp", "https", map[string]interface{}{"timeout": "10s"})
			},
			written: unstructuredOf(clientTrafficPolicyGVK, servicesNamespace, "ctp-ctp"),
			drift:   driftSpec,
		},
		{
			name: "backend traffic policy",
			run: func(ctx context.Context, c client.Client) error {
				return withClient(c).ensureKernelBackendTrafficPolicyNamed(ctx, "btp", map[string]interface{}{"timeout": "10s"})
			},
			written: unstructuredOf(backendTrafficPolicyGVK, servicesNamespace, "btp"),
			drift:   driftSpec,
		},
		{
			name: "kernel security policy",
			run: func(ctx context.Context, c client.Client) error {
				return withClient(c).ensureKernelSecurityPolicy(ctx, kernelHTTPRouteSpec{name: "route", securityPolicy: map[string]interface{}{"basicAuth": "x"}})
			},
			written: unstructuredOf(securityPolicyGVK, servicesNamespace, kernelSecurityPolicyName("route")),
			drift:   driftSpec,
		},
		{
			name: "zone secret",
			seed: []client.Object{zoneSecretSource()},
			run: func(ctx context.Context, c client.Client) error {
				comp := &gentianov1alpha1.Component{}
				comp.Name, comp.Namespace, comp.UID = "wiki", "tenant-demo", "uid"
				return (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureZoneSecret(ctx, comp, "demo", testZone())
			},
			written: namedObject(&corev1.Secret{}, "tenant-demo", testZone().secretName),
			drift: func(obj client.Object) {
				s := obj.(*corev1.Secret)
				s.Data = map[string][]byte{"x": []byte("y")}
				s.Labels = map[string]string{"drifted": "true"}
			},
		},
		{
			name: "registry credentials",
			seed: []client.Object{registrySource()},
			run: func(ctx context.Context, c client.Client) error {
				return (&TenantReconciler{Client: c}).ensureRegistryCredentials(ctx, tenant, "tenant-demo")
			},
			written: namedObject(&corev1.Secret{}, "tenant-demo", "registry-credentials"),
			drift: func(obj client.Object) {
				s := obj.(*corev1.Secret)
				s.Data = map[string][]byte{"x": []byte("y")}
				s.Labels = map[string]string{"drifted": "true"}
			},
		},
		{
			name: "tile catalog",
			run: func(ctx context.Context, c client.Client) error {
				return (&TileProjectionReconciler{Client: c}).writeTo(ctx, "tenant-demo", "rendered")
			},
			written: namedObject(&corev1.ConfigMap{}, "tenant-demo", tilecatalogue.ConfigMapName),
			drift: func(obj client.Object) {
				cm := obj.(*corev1.ConfigMap)
				cm.Data = map[string]string{"x": "y"}
				cm.Labels = map[string]string{"drifted": "true"}
			},
		},
		{
			name: "zone hosts",
			run: func(ctx context.Context, c client.Client) error {
				return (&TileProjectionReconciler{Client: c}).writeZoneHosts(ctx, "demo", []string{"a.example"}, []string{"x"}, nil, []string{"p"})
			},
			written: namedObject(&corev1.ConfigMap{}, layout.Namespace(layout.Control), zoneHostsConfigMapName("demo")),
			drift: func(obj client.Object) {
				cm := obj.(*corev1.ConfigMap)
				cm.Data = map[string]string{"x": "y"}
				cm.Labels = map[string]string{"drifted": "true"}
			},
		},
		{
			name: "privilege secret",
			run: func(ctx context.Context, c client.Client) error {
				s := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"k": []byte("v")}}
				s.Name, s.Namespace, s.Labels = "priv", "tenant-demo", map[string]string{"a": "b"}
				return (&TenantReconciler{Client: c}).applySecret(ctx, s)
			},
			written: namedObject(&corev1.Secret{}, "tenant-demo", "priv"),
			drift: func(obj client.Object) {
				s := obj.(*corev1.Secret)
				s.Data = map[string][]byte{"x": []byte("y")}
				s.Labels = map[string]string{"drifted": "true"}
			},
		},
		{
			name:    "gateway class",
			run:     func(ctx context.Context, c client.Client) error { return withClient(c).ensureGatewayClass(ctx) },
			written: namedObject(&gatewayv1.GatewayClass{}, "", GentianGatewayClassName),
			drift: func(obj client.Object) {
				obj.(*gatewayv1.GatewayClass).Spec.ControllerName = "else"
				obj.(*gatewayv1.GatewayClass).Spec.Description = nil
			},
		},
		{
			name: "component policy",
			run: func(ctx context.Context, c client.Client) error {
				np := &networkingv1.NetworkPolicy{}
				np.Name, np.Namespace, np.Labels = "np", "tenant-demo", map[string]string{"a": "b"}
				np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
				comp := &gentianov1alpha1.Component{}
				comp.Name, comp.Namespace, comp.UID = "wiki", "tenant-demo", "uid"
				return (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).applyComponentPolicy(ctx, comp, np)
			},
			written: namedObject(&networkingv1.NetworkPolicy{}, "tenant-demo", "np"),
			drift: func(obj client.Object) {
				np := obj.(*networkingv1.NetworkPolicy)
				np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
				np.Labels = map[string]string{"drifted": "true"}
			},
		},
	}
	holdToTranscript(t, *updateTranscripts, "ensure", ensureTranscript(t, cases))
}
