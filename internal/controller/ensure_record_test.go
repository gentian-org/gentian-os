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
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
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
	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

// ensureScheme knows every kind an ensure below writes.
func ensureScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := SchemeForTest(t)
	for _, gvk := range []schema.GroupVersionKind{securityPolicyGVK, clientTrafficPolicyGVK, backendTrafficPolicyGVK, referenceGrantGVK, httpRouteFilterGVK,
		certManagerCertGVK, backendTLSPolicyGVK, kyvernoPolicyGVK, dnsEndpointGVK, externalSecretGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

// ensureCase is one ensure: what it writes on an empty cluster, on the
// cluster it left, and after drift changed what it wrote, and what it says
// when the write before the first and the one after drift are turned down.
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

// errEnsureRefused is what a write turned down answers.
var errEnsureRefused = errors.New("refused")

func ensureTranscript(t *testing.T, cases []ensureCase) string {
	t.Helper()
	var out []string
	for _, c := range cases {
		var calls []string
		record := func(verb string, obj client.Object) {
			calls = append(calls, fmt.Sprintf("%s %s/%s", verb, obj.GetNamespace(), obj.GetName()))
		}
		// refuse turns every write down, to show what an ensure says when
		// one fails.
		refuse := false
		cl := fake.NewClientBuilder().WithScheme(ensureScheme(t)).WithObjects(c.seed...).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				record("create", obj)
				if refuse {
					return errEnsureRefused
				}
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				record("update", obj)
				if refuse {
					return errEnsureRefused
				}
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				data, _ := patch.Data(obj)
				record("patch "+string(patch.Type())+" "+string(data), obj)
				if refuse {
					return errEnsureRefused
				}
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
		refuse = true
		step("refused on an empty cluster")
		refuse = false
		step("first")
		step("again")
		obj := c.written.DeepCopyObject().(client.Object)
		if err := cl.Get(ctx, client.ObjectKeyFromObject(c.written), obj); err == nil {
			c.drift(obj)
			if err := cl.Update(ctx, obj); err != nil {
				t.Fatal(err)
			}
		}
		refuse = true
		step("refused after drift")
		refuse = false
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

func testComponent() *gentianov1alpha1.Component {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "wiki", "tenant-demo", "uid"
	return comp
}

func testProfile() *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Name = "wiki"
	return p
}

func testTenant() *gentianov1alpha1.Tenant {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	return tenant
}

func turnEdgeService() *corev1.Service {
	svc := &corev1.Service{}
	svc.Name, svc.Namespace = turnEdgeName, turnEdgeNamespace
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.9"}}
	return svc
}

func driftNetworkPolicy(obj client.Object) {
	np := obj.(*networkingv1.NetworkPolicy)
	np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
	np.Labels = map[string]string{"drifted": "true"}
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
		{
			name: "argocd server certificate",
			run:  func(ctx context.Context, c client.Client) error { return withClient(c).ensureArgoCDBackendTLS(ctx) },
			written: unstructuredOf(certManagerCertGVK, argocdNamespace,
				buildArgoCDServerCertificate(argocdNamespace).GetName()),
			drift: driftSpec,
		},
		{
			name: "argocd backend TLS policy",
			run:  func(ctx context.Context, c client.Client) error { return withClient(c).ensureArgoCDBackendTLS(ctx) },
			written: unstructuredOf(backendTLSPolicyGVK, argocdNamespace,
				buildArgoCDBackendTLSPolicy(argocdNamespace).GetName()),
			drift: driftSpec,
		},
		{
			name: "log destination certificate",
			run: func(ctx context.Context, c client.Client) error {
				return (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).applyLogDestinationCertificate(ctx, testComponent(),
					&gentianov1alpha1.LogDestinationRequirement{Service: "syslog", Port: 6514})
			},
			written: unstructuredOf(certManagerCertGVK, buildLogDestinationCertificate(testComponent(), &gentianov1alpha1.LogDestinationRequirement{}).GetNamespace(),
				logDestinationCertificate),
			drift: driftSpec,
		},
		{
			name: "log destination policy",
			run: func(ctx context.Context, c client.Client) error {
				return (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).applyLogDestinationPolicy(ctx, testComponent(), "demo", testProfile(), 6514)
			},
			written: namedObject(&networkingv1.NetworkPolicy{}, "tenant-demo", logDestinationPolicyName(testComponent())),
			drift:   driftNetworkPolicy,
		},
		{
			name: "image pins",
			run: func(ctx context.Context, c client.Client) error {
				p := testProfile()
				p.Spec.Package.Images = []gentianov1alpha1.PinnedImage{{Repository: "registry.example/wiki", Digest: "sha256:0"}}
				return (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureImagePins(ctx, testComponent(), p)
			},
			written: unstructuredOf(kyvernoPolicyGVK, "tenant-demo", imagePinsName(testComponent())),
			drift:   driftSpec,
		},
		{
			name: "job secret",
			run: func(ctx context.Context, c client.Client) error {
				job := &batchv1.Job{}
				job.Name, job.Namespace, job.UID, job.Labels = "role", "tenant-demo", "job-uid", map[string]string{"a": "b"}
				return ensureJobSecret(ctx, c, job, jobSecret(job, map[string][]byte{"password": []byte("p")}))
			},
			written: namedObject(&corev1.Secret{}, "tenant-demo", "role"),
			drift: func(obj client.Object) {
				s := obj.(*corev1.Secret)
				s.Data = map[string][]byte{"x": []byte("y")}
				s.Labels = map[string]string{"drifted": "true"}
				s.OwnerReferences = nil
			},
		},
		{
			name: "turn DNS endpoint",
			seed: []client.Object{turnEdgeService()},
			run: func(ctx context.Context, c client.Client) error {
				return (&TURNEdgeReconciler{Client: c, KernelDomain: "example.org"}).ensureDNS(ctx)
			},
			written: unstructuredOf(dnsEndpointGVK, turnEdgeNamespace, turnEdgeName),
			drift:   driftSpec,
		},
		{
			name: "mail password file",
			run: func(ctx context.Context, c client.Client) error {
				return (&TenantReconciler{Client: c}).upsertSecret(ctx, "passwd", "kernel-mail", map[string][]byte{"demo.users": []byte("a@demo:{X}h")})
			},
			written: namedObject(&corev1.Secret{}, "kernel-mail", "passwd"),
			drift: func(obj client.Object) {
				s := obj.(*corev1.Secret)
				s.Data = map[string][]byte{"x": []byte("y")}
				s.Labels = map[string]string{"drifted": "true"}
			},
		},
		{
			name: "component egress policy",
			run: func(ctx context.Context, c client.Client) error {
				return (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureNetworkPolicy(ctx, testComponent(), testProfile(), testTenant(), true)
			},
			written: namedObject(&networkingv1.NetworkPolicy{}, "tenant-demo", componentNetworkPolicyName(testComponent())),
			drift:   driftNetworkPolicy,
		},
		{
			name: "tenant policy",
			run: func(ctx context.Context, c client.Client) error {
				np := &networkingv1.NetworkPolicy{}
				np.Name, np.Namespace, np.Labels = "tnp", "tenant-demo", map[string]string{"a": "b"}
				np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
				return (&TenantReconciler{Client: c}).applyTenantNetworkPolicy(ctx, np)
			},
			written: namedObject(&networkingv1.NetworkPolicy{}, "tenant-demo", "tnp"),
			drift:   driftNetworkPolicy,
		},
		{
			name: "run destination egress",
			run: func(ctx context.Context, c client.Client) error {
				run := &gentianov1alpha1.TenantExport{}
				run.Name, run.Namespace, run.UID = "exp", "tenant-demo", "run-uid"
				job := &batchv1.Job{}
				job.Namespace = "tenant-demo"
				return ensureRunDestinationEgress(ctx, c, c.Scheme(), run, "export",
					&gentianov1alpha1.BundleRef{Endpoint: "https://203.0.113.7:9000"}, []captureUnit{{Job: job}})
			},
			written: namedObject(&networkingv1.NetworkPolicy{}, "tenant-demo", runEgressPolicyName("export", "exp")),
			drift:   driftNetworkPolicy,
		},
		{
			name: "database secret",
			run: func(ctx context.Context, c client.Client) error {
				ready, reason, message, err := (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureDatabaseSecret(ctx, testComponent(), "demo", "store",
					map[string]interface{}{"url": "{{ .password }}"}, []interface{}{map[string]interface{}{"secretKey": "password"}})
				return fmt.Errorf("answered ready=%v reason=%q message=%q, error %v", ready, reason, message, err)
			},
			written: unstructuredOf(externalSecretGVK, "tenant-demo", testComponent().Name+componentDatabaseSecretSuffix),
			drift:   driftSpec,
		},
		{
			name: "http route",
			run: func(ctx context.Context, c client.Client) error {
				route := &gatewayv1.HTTPRoute{}
				route.Name, route.Namespace = "route", "tenant-demo"
				route.Labels, route.Annotations = map[string]string{"a": "b"}, map[string]string{"k": "v"}
				route.Spec.Hostnames = []gatewayv1.Hostname{"wiki.example.org"}
				return ensureHTTPRouteResource(ctx, c, route)
			},
			written: namedObject(&gatewayv1.HTTPRoute{}, "tenant-demo", "route"),
			drift: func(obj client.Object) {
				r := obj.(*gatewayv1.HTTPRoute)
				r.Spec.Hostnames = []gatewayv1.Hostname{"else.example.org"}
				r.Labels = map[string]string{"drifted": "true"}
				r.Annotations = map[string]string{"k": "old", "theirs": "kept"}
			},
		},
		{
			name: "gateway",
			run: func(ctx context.Context, c client.Client) error {
				gw := &gatewayv1.Gateway{}
				gw.Name, gw.Namespace = "edge", "kernel-edge"
				gw.Labels, gw.Annotations = map[string]string{"a": "b"}, map[string]string{"k": "v"}
				gw.Spec.GatewayClassName = GentianGatewayClassName
				return ensureGatewayResource(ctx, c, gw)
			},
			written: namedObject(&gatewayv1.Gateway{}, "kernel-edge", "edge"),
			drift: func(obj client.Object) {
				g := obj.(*gatewayv1.Gateway)
				g.Spec.GatewayClassName = "else"
				g.Labels = map[string]string{"drifted": "true"}
				g.Annotations = map[string]string{"k": "old", "theirs": "kept"}
			},
		},
		{
			name: "key list",
			run: func(ctx context.Context, c client.Client) error {
				return ensureKeyList(ctx, c, "keys", "keys.json", []componentkeys.Key{{Tenant: "demo", Component: "wiki", KeyHash: componentkeys.Hash("k")}})
			},
			written: namedObject(&corev1.ConfigMap{}, registrarRealmSecretNamespace(), "keys"),
			drift: func(obj client.Object) {
				cm := obj.(*corev1.ConfigMap)
				cm.Data = map[string]string{"x": "y"}
				cm.Labels = map[string]string{"drifted": "true"}
			},
		},
	}
	holdToTranscript(t, *updateTranscripts, "ensure", ensureTranscript(t, cases))
}
