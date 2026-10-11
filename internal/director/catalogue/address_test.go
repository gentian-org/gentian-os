/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package catalogue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// An address somebody typed must not be a way to reach inside the cluster or
// the network it is hosted on. What is refused as it is written, before
// anything is resolved:
func TestAnAddressThatIsNotAPublicHTTPSOneIsRefusedAsWritten(t *testing.T) {
	for address, why := range map[string]string{
		"http://catalogue.example.com/apps":               "only https",
		"ftp://catalogue.example.com/apps":                "only https",
		"//catalogue.example.com/apps":                    "only https",
		"https://user:secret@catalogue.example.com/a":     "contains",
		"https://catalogue.example.com/apps?x=1":          "contains",
		"https://catalogue.example.com/apps#x":            "contains",
		"https://catalogue.example.com:8443/apps":         "port 8443",
		"https://catalogue.example.com:6443/":             "port 6443",
		"https:///apps":                                   "no host",
		" https://catalogue.example.com/apps":             "whitespace",
		"":                                                "required",
		"https://director/":                               "single name",
		"https://kubernetes/api":                          "single name",
		"https://localhost/":                              "single name",
		"https://kubernetes.default.svc/api":              "(.svc)",
		"https://openbao.gentian-vault.svc/v1/sys":        "(.svc)",
		"https://keycloak.gentian-iam.svc.cluster.local":  "(.cluster.local)",
		"https://KEYCLOAK.gentian-iam.svc.Cluster.Local.": "(.cluster.local)",
		"https://printer.local/":                          "(.local)",
		"https://app.localhost/":                          "(.localhost)",
		"https://metadata.google.internal/":               "(.internal)",
		"https://127.0.0.1/":                              "127.0.0.0/8",
		"https://127.8.9.10/":                             "127.0.0.0/8",
		"https://[::1]/":                                  "::1/128",
		"https://0.0.0.0/":                                "0.0.0.0/8",
		"https://[::]/":                                   "::/128",
		"https://10.0.0.1/":                               "10.0.0.0/8",
		"https://10.43.0.1/":                              "10.0.0.0/8",
		"https://172.16.0.1/":                             "172.16.0.0/12",
		"https://172.31.255.254/":                         "172.16.0.0/12",
		"https://192.168.1.1/":                            "192.168.0.0/16",
		"https://100.64.0.1/":                             "100.64.0.0/10",
		"https://100.100.100.200/":                        "100.64.0.0/10",
		"https://169.254.169.254/latest/meta-data":        "169.254.0.0/16",
		"https://169.254.0.1/":                            "169.254.0.0/16",
		"https://[fe80::1]/":                              "fe80::/10",
		"https://[fd00:ec2::254]/":                        "fc00::/7",
		"https://[fc00::1]/":                              "fc00::/7",
		"https://224.0.0.1/":                              "224.0.0.0/4",
		"https://239.255.255.250/":                        "224.0.0.0/4",
		"https://[ff02::1]/":                              "ff00::/8",
		"https://255.255.255.255/":                        "240.0.0.0/4",
		"https://[::ffff:10.0.0.1]/":                      "10.0.0.0/8",
		"https://[::ffff:169.254.169.254]/":               "169.254.0.0/16",
		"https://[64:ff9b::a00:1]/":                       "64:ff9b::/96",
	} {
		err := CheckAddress(address)
		if !errors.Is(err, ErrAddressRefused) {
			t.Errorf("%q was not refused: %v", address, err)
			continue
		}
		if !strings.Contains(err.Error(), why) {
			t.Errorf("%q: the refusal does not say %q: %v", address, why, err)
		}
	}
}

func TestAPublicHTTPSAddressIsAccepted(t *testing.T) {
	for _, address := range []string{
		"https://gentian-org.github.io/gentian-apps",
		"https://catalogue.example.com",
		"https://catalogue.example.com/",
		"https://catalogue.example.com:443/apps/v1",
		"https://example.gitlab.io/team/catalogue",
		"https://93.184.216.34/apps",
		"https://[2606:2800:220:1:248:1893:25c8:1946]/apps",
	} {
		if err := CheckAddress(address); err != nil {
			t.Errorf("%q was refused: %v", address, err)
		}
	}
}

// resolves answers what a name resolves to, for a test.
type resolves map[string][]string

func (r resolves) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, a := range r[host] {
		out = append(out, netip.MustParseAddr(a))
	}
	if len(out) == 0 {
		return nil, errors.New("no such host")
	}
	return out, nil
}

// A name that looks public and resolves to something that is not: refused
// when the catalogue is added, with what it resolved to.
func TestAnAddressIsRefusedForWhatItsHostResolvesTo(t *testing.T) {
	dns := resolves{
		"public.example.com":    {"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"},
		"rebound.example.com":   {"10.43.0.1"},
		"metadata.example.com":  {"169.254.169.254"},
		"loop.example.com":      {"127.0.0.1"},
		"loop6.example.com":     {"::1"},
		"mixed.example.com":     {"93.184.216.34", "192.168.0.10"},
		"cgnat.example.com":     {"100.64.1.1"},
		"ula.example.com":       {"fd12:3456:789a::1"},
		"linklocal.example.com": {"fe80::1"},
		"zero.example.com":      {"0.0.0.0"},
		"multicast.example.com": {"224.0.0.251"},
		"mapped.example.com":    {"::ffff:10.0.0.1"},
	}
	if err := vet(context.Background(), dns, "https://public.example.com/apps"); err != nil {
		t.Fatalf("a public address was refused: %v", err)
	}
	for host := range dns {
		if host == "public.example.com" {
			continue
		}
		if err := vet(context.Background(), dns, "https://"+host+"/apps"); !errors.Is(err, ErrAddressRefused) {
			t.Errorf("%s resolves to %v and was not refused: %v", host, dns[host], err)
		}
	}
	if err := vet(context.Background(), dns, "https://nowhere.example.com/"); !errors.Is(err, ErrAddressRefused) {
		t.Errorf("a host that does not resolve was not refused: %v", err)
	}
	// What is wrong as written is refused before anything is looked up.
	if err := vet(context.Background(), resolves{}, "https://kubernetes.default.svc/"); !errors.Is(err, ErrAddressRefused) {
		t.Errorf("a cluster-internal name was not refused: %v", err)
	}
}

// The check that cannot be argued with: the address the dialer is about to
// connect to. Whatever a name resolved to when the catalogue was added, this
// is what it resolves to now, and it is the address that is connected to.
func TestTheDialerRefusesWhatItWouldConnectTo(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:443", "10.0.0.1:443", "172.20.0.1:443", "192.168.1.1:443", "100.64.0.1:443",
		"169.254.169.254:443", "0.0.0.0:443", "224.0.0.1:443",
		"[::1]:443", "[fe80::1]:443", "[fd00:ec2::254]:443", "[::ffff:127.0.0.1]:443", "[::]:443",
		"93.184.216.34:6443", // a public address, and not the https port
	} {
		network := "tcp4"
		if strings.HasPrefix(address, "[") {
			network = "tcp6"
		}
		if err := guardedDial(network, address, nil); !errors.Is(err, ErrAddressRefused) {
			t.Errorf("a connection to %s was not refused: %v", address, err)
		}
	}
	if err := guardedDial("tcp4", "93.184.216.34:443", nil); err != nil {
		t.Errorf("a connection to a public address was refused: %v", err)
	}
	if err := guardedDial("udp4", "93.184.216.34:443", nil); !errors.Is(err, ErrAddressRefused) {
		t.Errorf("a connection that is not TCP was not refused: %v", err)
	}
}

// The fetcher as the director builds it, pointed at a catalogue on this
// machine: it does not connect. A served profile and a served index are both
// refused, and the server never sees a request.
func TestTheFetcherDoesNotConnectInsideTheNetwork(t *testing.T) {
	asked := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		_, _ = w.Write([]byte("entries: []\n"))
	}))
	t.Cleanup(srv.Close)
	f := NewFetcher()
	src := Source{Key: "tenant/demo/inside", Name: "inside", URL: srv.URL}
	if _, err := f.Index(context.Background(), src); !errors.Is(err, ErrAddressRefused) {
		t.Errorf("an index on a loopback address was fetched: %v", err)
	}
	if _, err := f.Fetch(context.Background(), src, "app", "sha256:"+strings.Repeat("a", 64)); !errors.Is(err, ErrAddressRefused) {
		t.Errorf("a profile on a loopback address was fetched: %v", err)
	}
	if err := f.Vet(context.Background(), srv.URL); !errors.Is(err, ErrAddressRefused) {
		t.Errorf("a loopback address was accepted for a new catalogue: %v", err)
	}
	if asked != 0 {
		t.Fatalf("the server was asked %d times", asked)
	}
}

// A public catalogue that redirects is not followed: the redirect is the way
// a public address hands a request to a private one.
func TestARedirectIsNotFollowed(t *testing.T) {
	inside := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inside++
		_, _ = w.Write([]byte("entries: []\n"))
	}))
	t.Cleanup(target.Close)
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(front.Close)

	f := NewFetcher()
	// The guard's own client with the test server's certificate trusted and
	// its loopback address admitted, so that what is tested is the redirect.
	guarded := guardedClient()
	plain := front.Client()
	guarded.Transport = plain.Transport
	f.Client = guarded
	src := Source{Key: "cluster/front", Name: "front", URL: front.URL}
	if _, err := f.Index(context.Background(), src); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("a redirected index was read: %v", err)
	}
	if _, err := f.Fetch(context.Background(), src, "app", "sha256:"+strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("a redirected profile was read: %v", err)
	}
	if inside != 0 {
		t.Fatalf("the redirect was followed %d times", inside)
	}
}
