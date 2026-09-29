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
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeDNS answers from a map keyed by "TYPE name"; a missing key is NXDOMAIN.
type fakeDNS struct {
	answers map[string][]string
	failAll bool
}

func (f *fakeDNS) lookup(typ, name string) ([]string, error) {
	if f.failAll {
		return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true}
	}
	if a, ok := f.answers[typ+" "+name]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeDNS) LookupHost(_ context.Context, name string) ([]string, error) {
	return f.lookup("A", name)
}

func (f *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	return f.lookup("TXT", name)
}

func (f *fakeDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	raw, err := f.lookup("MX", name)
	if err != nil {
		return nil, err
	}
	out := make([]*net.MX, 0, len(raw))
	for _, r := range raw {
		pref, host, _ := strings.Cut(r, " ")
		p := uint16(0)
		if pref == "10" {
			p = 10
		}
		out = append(out, &net.MX{Pref: p, Host: host})
	}
	return out, nil
}

// gaugeValue reads one series of the published gauge. Read through client_model
// rather than prometheus/testutil, which would add a module to go.mod for the
// sake of one float.
func gaugeValue(t *testing.T, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := dnsRecordPublished.WithLabelValues(labels...).Write(m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func dnsEndpointObject(name string, endpoints ...map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dnsEndpointGVK)
	obj.SetNamespace("platform-kernel")
	obj.SetName(name)
	items := make([]interface{}, 0, len(endpoints))
	for _, e := range endpoints {
		items = append(items, e)
	}
	_ = unstructured.SetNestedSlice(obj.Object, items, "spec", "endpoints")
	return obj
}

func ep(name, typ string, targets ...string) map[string]interface{} {
	ts := make([]interface{}, 0, len(targets))
	for _, t := range targets {
		ts = append(ts, t)
	}
	return map[string]interface{}{"dnsName": name, "recordType": typ, "targets": ts}
}

func newVerifier(t *testing.T, dns *fakeDNS, objs ...*unstructured.Unstructured) (*DNSPublicationVerifier, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(dnsEndpointGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(dnsEndpointGVK.GroupVersion().WithKind("DNSEndpointList"), &unstructured.UnstructuredList{})
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	rec := record.NewFakeRecorder(10)
	return &DNSPublicationVerifier{
		Client:   b.Build(),
		Recorder: rec,
		Grace:    10 * time.Minute,
		Resolve:  func(context.Context, string) (dnsResolver, error) { return dns, nil },
	}, rec
}

func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// The case this exists for: the kernel domain's SPF and MX were requested and
// never published, and nothing said so.
func TestDNSVerifierReportsRecordMissingPastGrace(t *testing.T) {
	obj := dnsEndpointObject("mail-kernel",
		ep("gentian.cloud", "TXT", "v=spf1 a:mail-egress.gentian.cloud -all"),
		ep("gentian.cloud", "MX", "0 mail.gentian.cloud"),
		ep("mail.gentian.cloud", "A", "83.228.248.255"),
	)
	dns := &fakeDNS{answers: map[string][]string{"A mail.gentian.cloud": {"83.228.248.255"}}}
	v, rec := newVerifier(t, dns, obj)
	start := time.Now()

	v.CheckOnce(context.Background(), start)
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Fatalf("reported on first sight, before the grace period: %v", ev)
	}

	v.CheckOnce(context.Background(), start.Add(11*time.Minute))
	ev := drainEvents(rec)
	if len(ev) != 1 {
		t.Fatalf("want one Event naming the missing records, got %v", ev)
	}
	for _, want := range []string{"DNSRecordNotPublished", "TXT gentian.cloud", "MX gentian.cloud"} {
		if !strings.Contains(ev[0], want) {
			t.Fatalf("event %q does not mention %q", ev[0], want)
		}
	}
	if strings.Contains(ev[0], "mail.gentian.cloud A") || strings.Contains(ev[0], "A mail.gentian.cloud") {
		t.Fatalf("event %q names a record that IS published", ev[0])
	}
	if got := gaugeValue(t, "platform-kernel", "mail-kernel", "gentian.cloud", "TXT"); got != 0 {
		t.Fatalf("gauge for the missing SPF = %v, want 0", got)
	}
	if gaugeValue(t, "platform-kernel", "mail-kernel", "mail.gentian.cloud", "A") != 1 {
		t.Fatalf("gauge for the published A record should be 1")
	}
}

// A record that is merely propagating must not be reported: an alert that fires
// for every new record teaches people to ignore it.
func TestDNSVerifierToleratesPropagationWithinGrace(t *testing.T) {
	obj := dnsEndpointObject("mail-corp", ep("corp.gentian.cloud", "MX", "0 mail.gentian.cloud"))
	dns := &fakeDNS{answers: map[string][]string{}}
	v, rec := newVerifier(t, dns, obj)
	start := time.Now()

	v.CheckOnce(context.Background(), start)
	v.CheckOnce(context.Background(), start.Add(5*time.Minute))
	// external-dns publishes it before the grace period ends.
	dns.answers["MX corp.gentian.cloud"] = []string{"0 mail.gentian.cloud."}
	v.CheckOnce(context.Background(), start.Add(9*time.Minute))
	v.CheckOnce(context.Background(), start.Add(30*time.Minute))

	if ev := drainEvents(rec); len(ev) != 0 {
		t.Fatalf("reported a record that published within the grace period: %v", ev)
	}
}

// A resolver outage is not every record vanishing at once. Reporting it as such
// would bury a real missing record under noise the first time DNS hiccups.
func TestDNSVerifierIgnoresLookupFailures(t *testing.T) {
	obj := dnsEndpointObject("mail-kernel", ep("gentian.cloud", "TXT", "v=spf1 -all"))
	dns := &fakeDNS{failAll: true}
	v, rec := newVerifier(t, dns, obj)
	start := time.Now()

	v.CheckOnce(context.Background(), start)
	v.CheckOnce(context.Background(), start.Add(time.Hour))

	if ev := drainEvents(rec); len(ev) != 0 {
		t.Fatalf("a failing resolver produced reports: %v", ev)
	}
}

// Resolvers hand values back in their own form: TXT without the zone-file
// quotes, MX hosts with a trailing dot. Those are the same records.
func TestDNSVerifierNormalisesAnswers(t *testing.T) {
	obj := dnsEndpointObject("mail-kernel",
		ep("gentian.cloud", "TXT", "v=spf1 a:mail-egress.gentian.cloud -all"),
		ep("gentian.cloud", "MX", "0 mail.gentian.cloud"),
	)
	dns := &fakeDNS{answers: map[string][]string{
		"TXT gentian.cloud": {`"v=spf1 a:mail-egress.gentian.cloud -all"`},
		"MX gentian.cloud":  {"0 MAIL.gentian.cloud."},
	}}
	v, rec := newVerifier(t, dns, obj)
	start := time.Now()
	v.CheckOnce(context.Background(), start)
	v.CheckOnce(context.Background(), start.Add(time.Hour))
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Fatalf("published records reported missing because of formatting: %v", ev)
	}
}

// A wrong value is as unpublished as an absent one: an SPF naming the wrong host
// fails at every receiver.
func TestDNSVerifierReportsWrongValue(t *testing.T) {
	obj := dnsEndpointObject("mail-kernel", ep("mail.gentian.cloud", "A", "83.228.248.255"))
	dns := &fakeDNS{answers: map[string][]string{"A mail.gentian.cloud": {"10.0.0.1"}}}
	v, rec := newVerifier(t, dns, obj)
	start := time.Now()
	v.CheckOnce(context.Background(), start)
	v.CheckOnce(context.Background(), start.Add(11*time.Minute))
	if ev := drainEvents(rec); len(ev) != 1 {
		t.Fatalf("a record serving the wrong address was not reported: %v", ev)
	}
}

func TestIsNotFoundDistinguishesAbsenceFromFailure(t *testing.T) {
	if !isNotFound(&net.DNSError{IsNotFound: true}) {
		t.Fatal("NXDOMAIN must count as not found")
	}
	if isNotFound(&net.DNSError{IsTimeout: true}) {
		t.Fatal("a timeout must not count as not found")
	}
	if isNotFound(errors.New("boom")) {
		t.Fatal("an arbitrary error must not count as not found")
	}
}
