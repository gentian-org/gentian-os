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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// statusVerifier is a verifier that knows the cluster's domains and writes the
// status by domain, over a client that holds Tenants, Gateways and routes.
func statusVerifier(t *testing.T, dns *fakeDNS, objs ...client.Object) (*DNSPublicationVerifier, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, gentianov1alpha1.AddToScheme, gatewayv1.Install} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	scheme.AddKnownTypeWithName(dnsEndpointGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(dnsEndpointGVK.GroupVersion().WithKind("DNSEndpointList"), &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&gatewayv1.Gateway{}).Build()
	return &DNSPublicationVerifier{
		Client: c, Recorder: record.NewFakeRecorder(10), Grace: 10 * time.Minute,
		Resolve:      func(context.Context, string) (dnsResolver, error) { return dns, nil },
		KernelDomain: "k.example", TenancyMode: "multi", KernelRealm: "kernel", Namespace: "kernel-control",
	}, c
}

func readDNSStatus(t *testing.T, c client.Client) DNSStatus {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "kernel-control", Name: DNSStatusConfigMapName}, cm); err != nil {
		t.Fatalf("the DNS status was not written: %v", err)
	}
	var out DNSStatus
	if err := json.Unmarshal([]byte(cm.Data[DNSStatusKey]), &out); err != nil {
		t.Fatalf("the DNS status does not parse: %v", err)
	}
	return out
}

func recordOf(t *testing.T, s DNSStatus, domain, name, typ string) DNSRecordReport {
	t.Helper()
	for _, d := range s.Domains {
		if d.Domain != domain {
			continue
		}
		for _, r := range d.Records {
			if r.Name == name && r.Type == typ {
				return r
			}
		}
	}
	t.Fatalf("no %s %s under %s in %+v", typ, name, domain, s)
	return DNSRecordReport{}
}

// The case this exists for: a tenant's own domain, whose bare name held
// another provider's mail records. The cluster's MX and SPF are reported as
// held by somebody else, with what is served instead, and the bare name's
// address as missing -- under that domain and that tenant, beside the
// cluster's own domain, whose records are present.
func TestTheDNSStatusSaysWhatEachDomainLacksAndWhy(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "gentian"
	tenant.Status.Domain = "gentian.org"
	mail := dnsEndpointObject("mail-gentian",
		ep("gentian.org", "MX", "0 mail.k.example"),
		ep("gentian.org", "TXT", "v=spf1 a:mail-egress.k.example -all"),
		ep("_dmarc.gentian.org", "TXT", "v=DMARC1; p=none"),
	)
	kernelMail := dnsEndpointObject("mail-kernel", ep("k.example", "MX", "0 mail.k.example"))
	gw := &gatewayv1.Gateway{}
	gw.Name, gw.Namespace = PerimeterGatewayName, servicesNamespace
	gw.Spec.GatewayClassName = GentianGatewayClassName
	gw.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "192.0.2.10"}}
	route := &gatewayv1.HTTPRoute{}
	route.Name, route.Namespace = "tenant-gentian-apex", servicesNamespace
	route.Spec.Hostnames = []gatewayv1.Hostname{"gentian.org", "www.gentian.org"}
	kernelRoute := &gatewayv1.HTTPRoute{}
	kernelRoute.Name, kernelRoute.Namespace = "kernel-apex", servicesNamespace
	kernelRoute.Spec.Hostnames = []gatewayv1.Hostname{"k.example"}

	dns := &fakeDNS{answers: map[string][]string{
		"MX gentian.org":         {"16 route1.mx.cloudflare.net."},
		"TXT gentian.org":        {"v=spf1 include:_spf.mx.cloudflare.net ~all", "google-site-verification=abc"},
		"TXT _dmarc.gentian.org": {"v=DMARC1; p=none"},
		"A www.gentian.org":      {"192.0.2.10"},
		"MX k.example":           {"0 mail.k.example"},
		"A k.example":            {"192.0.2.10"},
	}}
	v, c := statusVerifier(t, dns, tenant, gw, route, kernelRoute)
	for _, o := range []*unstructured.Unstructured{mail, kernelMail} {
		if err := c.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	v.CheckOnce(context.Background(), start)
	if r := recordOf(t, readDNSStatus(t, c), "gentian.org", "gentian.org", "MX"); r.State != dnsStatePending {
		t.Fatalf("within the grace period the MX is %q, want pending", r.State)
	}
	v.CheckOnce(context.Background(), start.Add(11*time.Minute))
	s := readDNSStatus(t, c)

	mx := recordOf(t, s, "gentian.org", "gentian.org", "MX")
	if mx.State != dnsStateHeldByOther || len(mx.Got) != 1 || !strings.HasSuffix(mx.Got[0], " route1.mx.cloudflare.net.") || mx.Since == "" {
		t.Fatalf("gentian.org MX = %+v, want held by the other provider's MX", mx)
	}
	if spf := recordOf(t, s, "gentian.org", "gentian.org", "TXT"); spf.State != dnsStateHeldByOther {
		t.Fatalf("gentian.org SPF = %+v, want held by the other provider's SPF", spf)
	}
	if dmarc := recordOf(t, s, "gentian.org", "_dmarc.gentian.org", "TXT"); dmarc.State != dnsStatePresent || dmarc.Since != "" {
		t.Fatalf("gentian.org DMARC = %+v, want present", dmarc)
	}
	if apex := recordOf(t, s, "gentian.org", "gentian.org", "A"); apex.State != dnsStateMissing || apex.Want[0] != "192.0.2.10" {
		t.Fatalf("gentian.org's bare name = %+v, want its address missing", apex)
	}
	if www := recordOf(t, s, "gentian.org", "www.gentian.org", "A"); www.State != dnsStatePresent {
		t.Fatalf("www.gentian.org = %+v, want present", www)
	}
	for _, d := range s.Domains {
		if d.Domain == "gentian.org" && d.Tenant != "gentian" {
			t.Fatalf("gentian.org is listed for tenant %q", d.Tenant)
		}
	}
	if kmx := recordOf(t, s, "k.example", "k.example", "MX"); kmx.State != dnsStatePresent {
		t.Fatalf("the cluster's own MX = %+v, want present", kmx)
	}
	if ka := recordOf(t, s, "k.example", "k.example", "A"); ka.State != dnsStatePresent {
		t.Fatalf("the cluster's bare name = %+v, want present", ka)
	}
}

// A TXT record is held by somebody else only when a value of the same kind is
// there: an unrelated TXT at the same name is no conflict.
func TestHeldByOtherComparesTXTByKind(t *testing.T) {
	spf := dnsRecord{Type: "TXT", Targets: []string{"v=spf1 a:mail-egress.k.example -all"}}
	if heldByOther(spf, []string{"google-site-verification=abc"}) {
		t.Fatal("an unrelated TXT counted as another SPF")
	}
	if !heldByOther(spf, []string{`"v=spf1 include:_spf.mx.cloudflare.net ~all"`}) {
		t.Fatal("another SPF did not count")
	}
	if heldByOther(dnsRecord{Type: "MX"}, nil) {
		t.Fatal("nothing served counted as held")
	}
	if !heldByOther(dnsRecord{Type: "A", Targets: []string{"192.0.2.10"}}, []string{"198.51.100.1"}) {
		t.Fatal("another address did not count")
	}
}
