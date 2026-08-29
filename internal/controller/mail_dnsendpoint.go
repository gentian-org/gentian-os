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

	corev1 "k8s.io/api/core/v1"
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

	// Preference 0, not 10, because external-dns cannot converge on anything else.
	//
	// Its Cloudflare provider loses the preference when it reads a record back:
	// a record Cloudflare serves as "10 mail.gtn.host" returns as
	// "0 mail.gtn.host". The desired value then never equals the observed one, so
	// every reconcile deletes and recreates the record — once a minute, forever.
	// Cloudflare applies those as two operations, leaving a brief window with no
	// MX at all, during which a sending server falls back to the tenant's A
	// record and reaches the portal instead of Postfix.
	//
	// 0 is what the provider reports regardless, so publishing it is what makes
	// the two agree. It costs nothing here: preference only orders one MX against
	// another, and each tenant domain has exactly one. A second MX would need
	// this revisited — and the upstream read fixed — because the ordering between
	// them would then be real.
	records := []interface{}{
		dnsEndpointRecord(domain, "MX", "0 "+mailHost),
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

// syncKernelMailDNS publishes the DKIM record for the kernel domain itself.
//
// Separate from the per-tenant endpoint because the kernel domain has no Tenant
// to hang off, and narrower: only the DKIM record. The kernel domain's MX, SPF
// and web records come from elsewhere, and republishing them here would let two
// owners write the same names.
func (r *TenantReconciler) syncKernelMailDNS(ctx context.Context, dkimPublicKey string) error {
	if r.KernelDomain == "" {
		return nil
	}
	ns := defaultServicesNamespace()

	// The two records are independent. This used to return early without a DKIM
	// key, which would now also withhold the address the MX points at -- and the
	// address is what inbound mail needs, whether or not anything signs yet.
	var records []interface{}
	if dkimPublicKey != "" {
		records = append(records, dnsEndpointRecord(
			postfixDKIMSelector+"._domainkey."+r.KernelDomain, "TXT",
			fmt.Sprintf("v=DKIM1; h=sha256; k=rsa; s=email; p=%s", dkimPublicKey)))
	}

	// The address the MX points at.
	//
	// Every tenant's MX names mail.<kernelDomain> — syncTenantMailDNS says so and
	// explains why it must resolve and must never be a CNAME. Nothing published
	// it. The MX therefore named a host with no address, which fails twice over:
	// a sender cannot deliver inbound mail at all, and "v=spf1 mx ~all" — the
	// fallback used when no egressHost is configured — resolves the MX to no
	// address and so authorises nothing, failing SPF by construction.
	//
	// Taken from the inbound Service rather than a configured value because that
	// is where the address actually lives: it is assigned by the cloud, can change
	// when the Service is recreated, and a literal in a claim would be one more
	// thing to remember to edit. Absent (no LoadBalancer, or none assigned yet)
	// publishes nothing rather than a wrong answer — a stale A record here points
	// the internet's mail at something that is not a mail server.
	if addr := r.kernelMailAddress(ctx); addr != "" {
		records = append(records, dnsEndpointRecord("mail."+r.KernelDomain, "A", addr))
	}

	if len(records) == 0 {
		return nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dnsEndpointGVK)
	obj.SetName("mail-kernel")
	obj.SetNamespace(ns)
	obj.SetLabels(map[string]string{managedByLabel: managedByValue})
	if err := unstructured.SetNestedSlice(obj.Object, records, "spec", "endpoints"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dnsEndpointGVK)
	err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: ns}, existing)
	if err != nil {
		// No external-dns on this cluster: the record stays manual rather than
		// failing the reconcile.
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

// kernelMailAddress is the address inbound mail arrives on: the external
// address of the Postfix SMTP Service.
//
// Empty whenever there is no answer to give — no such Service, not a
// LoadBalancer, or an address not yet assigned — so the caller publishes no
// record rather than a wrong one.
//
// A hostname is returned as-is for the caller to reject: an MX target must be
// an address record, so a LoadBalancer that publishes a hostname needs a CNAME
// this function must not silently pretend is an A.
func (r *TenantReconciler) kernelMailAddress(ctx context.Context) string {
	svc := &corev1.Service{}
	name := types.NamespacedName{
		Name:      envOrDefault("MAIL_SMTP_SERVICE", "postfix-"+envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev"))+"-smtp"),
		Namespace: defaultServicesNamespace(),
	}
	if err := r.Get(ctx, name, svc); err != nil {
		return ""
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
	}
	return ""
}
