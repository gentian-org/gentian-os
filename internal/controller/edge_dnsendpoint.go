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
	"sort"

	runtimeMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Edge hostnames, published as a DNSEndpoint for external-dns to reconcile —
// the same mechanism mail already uses, for the same reason.
//
// external-dns has two sources here. Its gateway-httproute source reads
// hostnames off HTTPRoutes and takes the target from the Gateway's status
// addresses, which is exactly right on a static-ip cluster and is what
// publishes that cluster's records today. A tunnelled Gateway never gets an
// address — traffic arrives through cloudflared, not a LoadBalancer — so that
// source has no target to publish and, in v0.21.0, deadlocks rather than
// skipping: it stopped syncing fourteen seconds after the kernel Gateway was
// created and never resumed, taking the crd source down with it, so even mail
// stopped publishing.
//
// So on a tunnel cluster the hostnames arrive here instead, fully specified.
// The operator knows every name it routes and the ingress knows what they must
// point at, which is everything a record needs; nothing has to be inferred
// from an address that will never exist.
//
// This stays provider-agnostic. A DNSEndpoint is external-dns's own vendor
// -neutral CR, so the same records publish through whichever of the eight
// providers a cluster uses. It is not the operator writing DNS: it is the
// operator declaring intent for the component that owns DNS.

// syncEdgeDNSEndpoint publishes one DNSEndpoint for the hostnames this cluster
// routes, when the ingress supplies a target to point them at.
//
// A cluster whose ingress supplies none — static-ip, where the LoadBalancer
// address is the target and the Gateway carries it — gets nothing from here.
// That path is unchanged and keeps working exactly as it does today.
func syncEdgeDNSEndpoint(
	ctx context.Context,
	c client.Client,
	ing EdgeIngress,
	namespace string,
	hostnames []string,
) error {
	target := edgeDNSTarget(ing)
	if target == "" || namespace == "" || len(hostnames) == 0 {
		return nil
	}

	// Sorted and de-duplicated: the record set is compared field by field on
	// every reconcile, so a set that reordered between passes would rewrite the
	// object forever.
	seen := map[string]struct{}{}
	uniq := make([]string, 0, len(hostnames))
	for _, h := range hostnames {
		if h == "" {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		uniq = append(uniq, h)
	}
	sort.Strings(uniq)

	records := make([]interface{}, 0, len(uniq))
	for _, h := range uniq {
		records = append(records, edgeDNSRecord(h, target, ing))
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dnsEndpointGVK)
	obj.SetName("edge-kernel")
	obj.SetNamespace(namespace)
	obj.SetLabels(map[string]string{managedByLabel: managedByValue})
	if err := unstructured.SetNestedSlice(obj.Object, records, "spec", "endpoints"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dnsEndpointGVK)
	err := c.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: namespace}, existing)
	if err != nil {
		// The CRD is absent until external-dns is installed. That is a cluster
		// without DNS automation rather than a fault, so the records stay
		// manual and the reconcile carries on — same rule as mail's.
		if runtimeMeta.IsNoMatchError(err) {
			ctrl.LoggerFrom(ctx).Info("DNSEndpoint CRD absent; edge records left to whoever publishes them here")
			return nil
		}
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return c.Create(ctx, obj)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

// edgeDNSTarget is what the ingress says its hostnames resolve to, read out of
// the annotations it already declares for external-dns. One source of truth:
// the value published here and the value a Gateway would carry cannot drift,
// because they are the same string.
func edgeDNSTarget(ing EdgeIngress) string {
	ann := edgeDNSAnnotations(ing)
	if ann == nil {
		return ""
	}
	return ann[edgeDNSTargetKey]
}

const edgeDNSTargetKey = "external-dns.alpha.kubernetes.io/target"

// edgeDNSRecord is dnsEndpointRecord plus the ingress's provider-specific
// settings, attached PER RECORD.
//
// The crd source does not read annotations off the DNSEndpoint object — that
// is how the service and ingress sources take their hints, and reusing the
// shape here looked right and did nothing. Provider settings reach the crd
// source only through spec.endpoints[].providerSpecific, which is why an
// object annotation asking Cloudflare to proxy these records produced five
// unproxied CNAMEs pointing at cfargotunnel.com — a target that resolves to
// nothing at all, so the names existed, answered no address, and served no
// certificate.
//
// external-dns uses the same key strings in both forms, so the ingress still
// declares them once and this only changes where they are carried. Everything
// except the target is a provider setting; the target is the record's own
// value and is already its targets field.
func edgeDNSRecord(name, target string, ing EdgeIngress) map[string]interface{} {
	rec := dnsEndpointRecord(name, "CNAME", target)

	ann := edgeDNSAnnotations(ing)
	keys := make([]string, 0, len(ann))
	for k := range ann {
		if k == edgeDNSTargetKey {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return rec
	}
	// Sorted, because this map is compared field by field on every reconcile
	// and Go randomises map iteration: unsorted, the object would be rewritten
	// on roughly every pass forever.
	sort.Strings(keys)

	ps := make([]interface{}, 0, len(keys))
	for _, k := range keys {
		ps = append(ps, map[string]interface{}{"name": k, "value": ann[k]})
	}
	rec["providerSpecific"] = ps
	return rec
}

// syncTenantEdgeDNSEndpoint is the per-tenant sibling of syncEdgeDNSEndpoint:
// the tenant's wildcard and apex, in an object named for the tenant so that
// removing the tenant removes exactly its records and nobody else's.
func syncTenantEdgeDNSEndpoint(
	ctx context.Context,
	c client.Client,
	ing EdgeIngress,
	tenantName string,
	hostnames []string,
) error {
	target := edgeDNSTarget(ing)
	if target == "" || tenantName == "" {
		return nil
	}

	records := make([]interface{}, 0, len(hostnames))
	for _, h := range hostnames {
		if h == "" {
			continue
		}
		records = append(records, edgeDNSRecord(h, target, ing))
	}
	if len(records) == 0 {
		return nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dnsEndpointGVK)
	obj.SetName("edge-" + tenantName)
	obj.SetNamespace(defaultServicesNamespace())
	obj.SetLabels(map[string]string{
		managedByLabel: managedByValue,
		tenantLabel:    tenantName,
	})
	if err := unstructured.SetNestedSlice(obj.Object, records, "spec", "endpoints"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dnsEndpointGVK)
	err := c.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if err != nil {
		if runtimeMeta.IsNoMatchError(err) {
			return nil
		}
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return c.Create(ctx, obj)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}
