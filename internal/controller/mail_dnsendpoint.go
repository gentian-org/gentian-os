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
// Skipped entirely when the CLUSTER does not run kernel mail: publishing an MX
// for a domain this cluster does not accept mail for points senders at a server
// that will refuse them, which is worse than no record at all.
//
// That sentence was the contract long before anything enforced it. The records
// were published for any tenant whose own spec.mail.mode said selfhosted, which
// is a statement about what the TENANT wants and not about what exists: on a
// cluster with mail.serviceMode external there is no Dovecot, no mailbox, and
// no mail.<kernelDomain> to point an MX at. Every tenant on such a cluster
// published `MX 0 mail.<kernelDomain>` naming a host with no address, and
// `v=spf1 mx ~all` authorising that same absent MX. Inbound mail was
// undeliverable and the SPF record authorised nothing.
//
// The gate is the same one Dovecot uses, for the same reason and read from the
// same place — see dovecotDeployed. Steps 4 to 6 of ensureMailSelfhosted were
// given it after they were found "provisioning into a void"; this step does the
// same thing to a public DNS zone and was missed.
//
// On a tunnel cluster it is worse than waste. The tenant's web address is a
// CNAME to the tunnel at exactly the name these records claim, and RFC 1034
// §3.6.2 forbids a CNAME beside any other type — so external-dns discards the
// CNAME, logs "conflicting record type candidates" once a minute, and the
// tenant's site stops resolving entirely. Mail that cannot work takes down a
// website that otherwise would.
//
// Records already published are removed rather than left: a cluster that
// switches to external, or a tenant that moves off kernel mail, must not keep
// an MX pointing at a server that no longer accepts for it.
func (r *TenantReconciler) syncTenantMailDNS(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	domain := mailDomain(tenant, r.KernelDomain, r.TenancyMode)
	if domain == "" || tenant.Status.Mail == nil {
		return nil
	}
	ns := defaultServicesNamespace()

	if !r.dovecotDeployed(ctx) {
		return r.deleteTenantMailDNS(ctx, tenant, ns)
	}

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

// deleteTenantMailDNS removes the tenant's mail DNSEndpoint, if one is there.
//
// Not a no-op on a cluster that never had kernel mail: a cluster whose
// serviceMode CHANGED, or a tenant moved to transport-only, still holds records
// published under the old answer, and external-dns keeps serving them until
// something withdraws the desired state. Leaving them is how a tenant's website
// stays down after the fault that took it down has been fixed.
//
// Absent CRD and absent object are both success: a cluster without external-dns
// keeps its records by hand, which is a missing convenience rather than a
// broken tenant -- the same reasoning the create path uses.
func (r *TenantReconciler) deleteTenantMailDNS(ctx context.Context, tenant *gentianov1alpha1.Tenant, ns string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dnsEndpointGVK)
	obj.SetName("mail-" + tenant.Name)
	obj.SetNamespace(ns)
	if err := r.Delete(ctx, obj); err != nil {
		if runtimeMeta.IsNoMatchError(err) || client.IgnoreNotFound(err) == nil {
			return nil
		}
		return err
	}
	return nil
}

// syncKernelMailDNS publishes the mail records for the kernel domain itself.
//
// Separate from the per-tenant endpoint because the kernel domain has no Tenant
// to hang off. It used to publish only DKIM, on the stated grounds that "the MX,
// SPF and web records come from elsewhere" — and for MX and SPF, nowhere was
// elsewhere. Every tenant domain had all three while the kernel domain had a
// DKIM key and nothing else, which is the worst of the available states: mail
// was signed as the kernel domain, no receiver was told which hosts may send as
// it, and no policy said what to do about the ones that are not. That is the
// domain the open-relay spam forged its senders in.
//
// Its MX was missing too, so mail addressed to the kernel domain fell back to
// the A record — the HTTP gateway, which is not a mail server. Postfix accepts
// the domain and Dovecot holds a maildir for it, so the mailbox existed and was
// merely unreachable. The DMARC record below names rua=dmarc@<kernelDomain>, so
// the MX is what makes those reports arrive rather than bounce.
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

	// SPF and DMARC for the kernel domain, on the same terms as every tenant's.
	//
	// Skipped when a Tenant already owns the domain — single-tenant clusters give
	// the tenant the kernel domain itself, and two endpoints writing one name is
	// how external-dns ends up flapping between two owners' ideas of it.
	ownedByTenant, err := r.kernelDomainOwnedByTenant(ctx)
	if err != nil {
		return err
	}
	if !ownedByTenant {
		records = append(records,
			dnsEndpointRecord(r.KernelDomain, "TXT", mailSPFRecord(
				clusterMailEgressHost(ctx, r.Client, envOrDefault("MAIL_EGRESS_HOST", "")))),
			dnsEndpointRecord("_dmarc."+r.KernelDomain, "TXT", mailDMARCRecord(r.KernelDomain)))
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
		// The kernel domain's own MX, gated on the same address for the same
		// reason the tenants' is: an MX naming a host with no address is worse
		// than no MX, because a sender keeps retrying instead of failing fast.
		//
		// Preference 0 — see syncTenantMailDNS for why anything else makes
		// external-dns delete and recreate the record once a minute.
		if !ownedByTenant {
			records = append(records, dnsEndpointRecord(r.KernelDomain, "MX", "0 mail."+r.KernelDomain))
		}
	}

	// Where a mail client fetches mail, published for the same reason as the MX
	// and taken from the same place — the Service the cloud assigned.
	//
	// It is a distinct name from mail.<domain> because it is a distinct load
	// balancer in front of a distinct workload: Dovecot serves IMAPS, Postfix
	// serves the MX, and a single name could only ever point at one of them.
	//
	// The name also has to be one the certificate covers, since a mail client
	// verifies it — which is why clients are told to use imap.<domain> rather
	// than the in-cluster Service name no public CA can sign for.
	if addr := r.kernelIMAPAddress(ctx); addr != "" {
		records = append(records, dnsEndpointRecord("imap."+r.KernelDomain, "A", addr))
	}

	// The forward half of forward-confirmed reverse DNS.
	//
	// A receiver checks that the sending address has a PTR naming a host, and
	// that the host resolves back to the same address. The PTR is set at the
	// cloud provider; this is the record it has to agree with, and until now it
	// was created by hand -- so the one record the platform did not own was the
	// one holding the pair together.
	//
	// The address comes from the node's ExternalIP, which is the floating IP the
	// provider reports once it is attached to that node's port. That is the same
	// address mail then leaves from, so the record cannot drift from reality the
	// way a literal on a claim can.
	//
	// Published only when the cluster names an egress host. Without one the
	// cluster sends from the shared address or relays through a smarthost, and
	// there is no name to publish.
	if egress := clusterMailEgressHost(ctx, r.Client, envOrDefault("MAIL_EGRESS_HOST", "")); egress != "" {
		if addr := r.mailEgressAddress(ctx); addr != "" {
			records = append(records, dnsEndpointRecord(egress, "A", addr))
		}
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
	err = r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: ns}, existing)
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

// kernelDomainOwnedByTenant reports whether some Tenant's mail domain IS the
// kernel domain, which is the normal arrangement on a single-tenant cluster.
//
// The per-tenant endpoint then already publishes the MX, SPF and DMARC for that
// name, and publishing them here as well would give one name two owners with no
// mechanism to agree — each reconcile overwriting the other's version.
func (r *TenantReconciler) kernelDomainOwnedByTenant(ctx context.Context) (bool, error) {
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return false, err
	}
	for i := range tenants.Items {
		if mailDomain(&tenants.Items[i], r.KernelDomain, r.TenancyMode) == r.KernelDomain {
			return true, nil
		}
	}
	return false, nil
}

// kernelIMAPAddress is the address mail clients fetch from: the external address
// of the Dovecot IMAPS Service.
//
// Empty when IMAPS is not published — imapIngress disabled, or the address not
// assigned yet — and the caller then publishes no record, which is the honest
// answer. A name that resolves to nothing tells a mail client to keep retrying;
// a name that resolves to the wrong host tells it to send credentials there.
func (r *TenantReconciler) kernelIMAPAddress(ctx context.Context) string {
	svc := &corev1.Service{}
	name := types.NamespacedName{
		Name:      envOrDefault("MAIL_IMAP_SERVICE", "dovecot-"+envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev"))+"-imaps"),
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

// mailEgressAddress is the address outbound mail leaves from: the ExternalIP the
// cloud provider reports on the node carrying the floating IP.
//
// Exactly one node must carry one. Zero means no dedicated egress has been
// attached yet; more than one means the cluster cannot say which address mail
// will leave from, and guessing publishes a record that is wrong half the time.
// Both return empty, and the caller publishes nothing rather than something
// unverifiable.
func (r *TenantReconciler) mailEgressAddress(ctx context.Context) string {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return ""
	}
	var found string
	for i := range nodes.Items {
		for _, a := range nodes.Items[i].Status.Addresses {
			if a.Type != corev1.NodeExternalIP || a.Address == "" {
				continue
			}
			if found != "" && found != a.Address {
				return ""
			}
			found = a.Address
		}
	}
	return found
}
