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
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"
)

// The kernel zone at the edge (networking.md §4): one confidential client,
// one cookie on the kernel domain, every kernel-zone route behind the same
// session. The client and its Secret are the identity bootstrap's; the
// policies that use them are reconciled here.
const (
	// edgeKernelClientID is the kernel zone's OIDC client in the kernel realm.
	edgeKernelClientID = "gentian-edge-kernel"
	// edgeKernelSecretName is the Secret in the edge namespace holding that
	// client's secret under the key Envoy Gateway reads.
	edgeKernelSecretName = "edge-kernel-oidc"
	// The zone's cookies. Named, and the same on every policy, so that the
	// routes share one session; Envoy Gateway would otherwise suffix each
	// policy's cookies with a hash of its own and give every host its own.
	edgeKernelAccessTokenCookie = "gentian-kernel-access"
	edgeKernelIDTokenCookie     = "gentian-kernel-id"
	// edgeAuthzRoutesConfigMap is the route table the ext-auth shim reads:
	// per host, the relation a caller must hold, written beside the routes.
	edgeAuthzRoutesConfigMap = "edge-authz-routes"
	edgeAuthzRoutesKey       = "routes.yaml"
	edgeAuthzPort            = int32(9001)
)

// routeAuthz is what a route's exposure says must hold at L2.
type routeAuthz struct {
	relation string
	object   string
	// forwardToken hands the edge token to the backend. Only the desktop,
	// which relays to the director, declares it (AD-13).
	forwardToken bool
}

var securityPolicyGVK = schema.GroupVersionKind{
	Group:   "gateway.envoyproxy.io",
	Version: "v1alpha1",
	Kind:    "SecurityPolicy",
}

// kernelZoneReady reports whether the kernel zone's client secret exists. A
// route behind the zone session is not created before it: a SecurityPolicy
// naming a missing Secret is invalid, and an invalid policy leaves its route
// served with no policy at all -- so until the identity bootstrap has run,
// the kernel UIs have no route rather than an open one.
func (r *GatewayPlatformReconciler) kernelZoneReady(ctx context.Context) bool {
	return kernelZoneReadyWith(ctx, r.Client)
}

func kernelZoneReadyWith(ctx context.Context, c client.Reader) bool {
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: edgeKernelSecretName, Namespace: servicesNamespace}, secret)
	return err == nil && len(secret.Data["client-secret"]) > 0
}

func kernelSecurityPolicyName(route string) string { return "sp-" + route }

// kernelSecurityPolicySpec is L1 and L2 for one kernel-zone route: the
// zone's session (OIDC against the kernel realm with the zone client,
// cookie on the kernel domain) and the ext-auth shim, which fails closed.
func kernelSecurityPolicySpec(kernelDomain, kernelRealm, route string, authz routeAuthz, shimService string) map[string]interface{} {
	return map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": gatewayv1.GroupName,
				"kind":  "HTTPRoute",
				"name":  route,
			},
		},
		"oidc": map[string]interface{}{
			"provider": map[string]interface{}{
				"issuer": fmt.Sprintf("https://id.%s/auth/realms/%s", kernelDomain, kernelRealm),
			},
			"clientID":     edgeKernelClientID,
			"clientSecret": map[string]interface{}{"name": edgeKernelSecretName},
			"logoutPath":   "/oauth2/logout",
			"cookieDomain": kernelDomain,
			"cookieNames": map[string]interface{}{
				"accessToken": edgeKernelAccessTokenCookie,
				"idToken":     edgeKernelIDTokenCookie,
			},
			"forwardAccessToken": authz.forwardToken,
			"scopes":             []interface{}{"openid", "profile", "email"},
			"refreshToken":       true,
		},
		"extAuth": map[string]interface{}{
			"failOpen": false,
			"grpc": map[string]interface{}{
				"backendRef": map[string]interface{}{
					"name": shimService,
					"port": int64(edgeAuthzPort),
				},
			},
		},
	}
}

func (r *GatewayPlatformReconciler) ensureKernelSecurityPolicy(ctx context.Context, spec kernelHTTPRouteSpec) error {
	if spec.authz == nil {
		return nil
	}
	name := kernelSecurityPolicyName(spec.name)
	policySpec := kernelSecurityPolicySpec(r.KernelDomain, r.kernelRealm(), spec.name, *spec.authz, r.edgeAuthzService())
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(securityPolicyGVK)
	desired.SetName(name)
	desired.SetNamespace(servicesNamespace)
	desired.SetLabels(map[string]string{
		managedByLabel:        managedByValue,
		gatewayComponentLabel: gatewayComponentKernel,
	})
	if err := unstructured.SetNestedField(desired.Object, policySpec, "spec"); err != nil {
		return err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(securityPolicyGVK)
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, policySpec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func (r *GatewayPlatformReconciler) deleteStaleKernelSecurityPolicies(ctx context.Context, expected map[string]struct{}) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: securityPolicyGVK.Group, Version: securityPolicyGVK.Version, Kind: "SecurityPolicyList"})
	if err := r.List(ctx, list,
		client.InNamespace(servicesNamespace),
		client.MatchingLabels{managedByLabel: managedByValue, gatewayComponentLabel: gatewayComponentKernel},
	); err != nil {
		return err
	}
	for i := range list.Items {
		if _, ok := expected[list.Items[i].GetName()]; ok {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// edgeAuthzRoute is one line of the shim's table (internal/edge/authz).
type edgeAuthzRoute struct {
	Host              string `json:"host"`
	Relation          string `json:"relation"`
	Object            string `json:"object"`
	AccessTokenCookie string `json:"accessTokenCookie,omitempty"`
	ForwardToken      bool   `json:"forwardToken,omitempty"`
	AuthMode          string `json:"authMode"`
}

// edgeAuthzRouteTable renders the shim's table from the routes that carry an
// L2 question. Sorted by host: one table for one state, however the specs
// were listed.
func edgeAuthzRouteTable(specs []kernelHTTPRouteSpec) (string, error) {
	var routes []edgeAuthzRoute
	for _, s := range specs {
		if s.authz == nil || s.host == "" {
			continue
		}
		routes = append(routes, edgeAuthzRoute{
			Host: s.host, Relation: s.authz.relation, Object: s.authz.object,
			AccessTokenCookie: edgeKernelAccessTokenCookie, ForwardToken: s.authz.forwardToken,
			AuthMode: "oidc",
		})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Host < routes[j].Host })
	b, err := yaml.Marshal(map[string]interface{}{"routes": routes})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *GatewayPlatformReconciler) ensureEdgeAuthzRouteTable(ctx context.Context, specs []kernelHTTPRouteSpec) error {
	table, err := edgeAuthzRouteTable(specs)
	if err != nil {
		return err
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      edgeAuthzRoutesConfigMap,
			Namespace: servicesNamespace,
			Labels: map[string]string{
				managedByLabel:        managedByValue,
				gatewayComponentLabel: gatewayComponentKernel,
			},
		},
		Data: map[string]string{edgeAuthzRoutesKey: table},
	}
	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Data[edgeAuthzRoutesKey] == table {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	return r.Patch(ctx, existing, patch)
}

func (r *GatewayPlatformReconciler) kernelRealm() string {
	if r.KernelRealm == "" {
		return "kernel"
	}
	return r.KernelRealm
}

func (r *GatewayPlatformReconciler) edgeAuthzService() string {
	if r.EdgeAuthzService == "" {
		return "gentian-os-edge-authz"
	}
	return r.EdgeAuthzService
}
