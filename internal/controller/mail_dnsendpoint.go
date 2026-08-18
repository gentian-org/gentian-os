// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"

	runtimeMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Mail DNS records, published as a DNSEndpoint for external-dns to reconcile.
//
// The web addresses need nothing here: external-dns reads them from the
// HTTPRoutes the operator already writes. Mail records have no HTTP object
// behind them, so they arrive through the CRD source instead.
//
// Publishing them rather than leaving them to an operator's hand also removes a
// failure that has already happened once: a wildcard only covers names with no
// records of their own, so adding an MX at a tenant domain silently stopped it
// inheriting *.<kernel-domain> and took its web addresses offline. With every
// name explicit, nothing depends on wildcard coverage.
var dnsEndpointGVK = schema.GroupVersionKind{
	Group:   "externaldns.k8s.io",
	Version: "v1alpha1",
	Kind:    "DNSEndpoint",
}

func dnsEndpointRecord(name, recordType string, targets ...string) map[string]interface{} {
	t := make([]interface{}, 0, len(targets))
	for _, x := range targets {
		t = append(t, x)
	}
	return map[string]interface{}{
		"dnsName":    name,
		"recordType": recordType,
		"recordTTL":  int64(300),
		"targets":    t,
	}
}

// syncTenantMailDNS writes one DNSEndpoint holding every mail record a tenant
// domain needs.
//
// Skipped entirely when the tenant is not on kernel mail: publishing an MX for
// a domain this cluster does not accept mail for points senders at a server
// that will refuse them, which is worse than no record at all.
func (r *TenantReconciler) syncTenantMailDNS(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	domain := mailDomain(tenant, r.KernelDomain, r.TenancyMode)
	if domain == "" || tenant.Status.Mail == nil {
		return nil
	}
	ns := defaultServicesNamespace()

	// The MX target is the kernel's mail host, not the tenant's own domain: one
	// Postfix serves every tenant, so they all point at the same name. That name
	// must resolve to an address and must never be a CNAME — an MX pointing at a
	// CNAME is invalid and silently ignored by many senders.
	mailHost := "mail." + r.KernelDomain

	records := []interface{}{
		dnsEndpointRecord(domain, "MX", "10 "+mailHost),
	}
	if v := tenant.Status.Mail.SPFRecord; v != "" {
		records = append(records, dnsEndpointRecord(domain, "TXT", v))
	}
	if v := tenant.Status.Mail.DMARCRecord; v != "" {
		records = append(records, dnsEndpointRecord("_dmarc."+domain, "TXT", v))
	}
	// The selector matches the one Postfix signs with; a mismatch here means a
	// signature nobody can verify, which reads as "DKIM broken" rather than as a
	// naming disagreement.
	if v := tenant.Status.Mail.DKIMPublicKey; v != "" {
		records = append(records, dnsEndpointRecord(
			postfixDKIMSelector+"._domainkey."+domain, "TXT",
			fmt.Sprintf("v=DKIM1; h=sha256; k=rsa; s=email; p=%s", v)))
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dnsEndpointGVK)
	obj.SetName("mail-" + tenant.Name)
	obj.SetNamespace(ns)
	obj.SetLabels(map[string]string{managedByLabel: managedByValue})
	if err := unstructured.SetNestedSlice(obj.Object, records, "spec", "endpoints"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dnsEndpointGVK)
	err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: ns}, existing)
	if err != nil {
		// The CRD is absent until external-dns is installed. That is a cluster
		// without DNS automation, not a broken tenant, so it must not fail the
		// reconcile — the records simply stay manual.
		if runtimeMeta.IsNoMatchError(err) {
			return nil
		}
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return r.Create(ctx, obj)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, obj)
}
