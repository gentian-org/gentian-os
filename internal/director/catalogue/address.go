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
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// A catalogue's address is something a person typed, and the director that
// fetches from it runs in the control namespace: beside the operator, the
// identity provider and the secret store, on a node of somebody's network.
// Left unchecked, "add a catalogue" would be "make the director send a
// request wherever I say" -- to a Service of the cluster, to the node's
// metadata endpoint, to a host of the hosting network.
//
// So an address is one a stranger on the internet could have fetched from,
// or it is refused. Three checks, each where it can be made:
//
//   - what is written: https, a public-looking host name, no credentials, no
//     port but 443, nothing after the path (CheckAddress);
//   - what the name resolves to, when the catalogue is added, so a typo is
//     told to the person who made it rather than found at the first install
//     (Vet);
//   - what is connected to, every time: the address is checked in the dialer,
//     after resolution and immediately before the connection is made, so the
//     address checked IS the address connected to. A name that resolved to
//     something public when it was added and to 10.0.0.1 now is refused now.
//
// Redirects are not followed, so a public address cannot hand the request on
// to a private one. No credential is ever sent: there is none to send.

// ErrAddressRefused is an address the director will not fetch from.
var ErrAddressRefused = errors.New("catalogue: the address is refused")

// refusedNets are the destinations a catalogue is never fetched from: every
// range that is not the public internet.
var refusedNets = func() []netip.Prefix {
	var out []netip.Prefix
	for _, cidr := range []string{
		"0.0.0.0/8",       // "this network", and the unspecified address
		"10.0.0.0/8",      // private (RFC 1918)
		"100.64.0.0/10",   // carrier-grade NAT (RFC 6598); also where some clouds keep metadata
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, where 169.254.169.254 serves instance metadata
		"172.16.0.0/12",   // private (RFC 1918)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"192.168.0.0/16",  // private (RFC 1918)
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, and the broadcast address
		"::/128",          // unspecified
		"::1/128",         // loopback
		"64:ff9b::/96",    // NAT64: an IPv4 address behind a prefix
		"64:ff9b:1::/48",  // local-use NAT64
		"100::/64",        // discard
		"2001::/23",       // IETF protocol assignments, Teredo among them
		"2001:db8::/32",   // documentation
		"2002::/16",       // 6to4: an IPv4 address behind a prefix
		"fc00::/7",        // unique local, where fd00:ec2::254 serves instance metadata
		"fe80::/10",       // link-local
		"ff00::/8",        // multicast
	} {
		out = append(out, netip.MustParsePrefix(cidr))
	}
	return out
}()

// refusedIP says why an address is not one a catalogue is fetched from, or "".
func refusedIP(addr netip.Addr) string {
	if !addr.IsValid() {
		return "it is not an address"
	}
	// ::ffff:10.0.0.1 is 10.0.0.1, whatever it is written as.
	addr = addr.Unmap().WithZone("")
	for _, n := range refusedNets {
		if n.Contains(addr) {
			return "it is in " + n.String() + ", which is not a public address"
		}
	}
	return ""
}

// internalSuffixes are the names that only mean something inside a cluster or
// on the machine itself.
var internalSuffixes = []string{".svc", ".cluster.local", ".local", ".localhost", ".internal"}

// CheckAddress says what is wrong with a catalogue address as it is written,
// or nil. It resolves nothing.
func CheckAddress(raw string) error {
	refuse := func(why string) error {
		return fmt.Errorf("%w: %s", ErrAddressRefused, why)
	}
	if raw == "" || strings.TrimSpace(raw) != raw || len(raw) > 2048 {
		return refuse("an address is required, without leading or trailing whitespace")
	}
	// Only what a plain address is made of. It is written into a manifest in
	// git, so it is matched rather than escaped.
	for _, c := range raw {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~/:%+[]", c) {
			return refuse(fmt.Sprintf("it contains %q; an address is https://<host>/<path>", c))
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return refuse("it is not a URL")
	}
	if u.Scheme != "https" {
		return refuse("only https addresses are fetched from")
	}
	if u.User != nil {
		return refuse("it carries a user name or password; a catalogue is public, and no credential is sent to one")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return refuse("it has a query or a fragment; an address is https://<host>/<path>")
	}
	if port := u.Port(); port != "" && port != "443" {
		return refuse("it names port " + port + "; a catalogue is served on the https port, 443")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return refuse("it names no host")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if why := refusedIP(addr); why != "" {
			return refuse("its host is " + host + ": " + why)
		}
		return nil
	}
	if !strings.Contains(host, ".") {
		return refuse("its host " + host + " is a single name, which only resolves inside a cluster or a local network")
	}
	for _, suffix := range internalSuffixes {
		if host == suffix[1:] || strings.HasSuffix(host, suffix) {
			return refuse("its host " + host + " is a name inside a cluster or a local network (" + suffix + ")")
		}
	}
	return nil
}

// Resolver is the part of net.Resolver an address check needs.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// vet is CheckAddress and then what the host resolves to: every address it
// has must be a public one. One private address among several public ones is
// refused, because which of them is connected to is not the caller's choice.
func vet(ctx context.Context, resolver Resolver, raw string) error {
	if err := CheckAddress(raw); err != nil {
		return err
	}
	u, _ := url.Parse(raw)
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return nil // an address, already checked
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("%w: its host %s does not resolve", ErrAddressRefused, host)
	}
	for _, addr := range addrs {
		if why := refusedIP(addr); why != "" {
			return fmt.Errorf("%w: its host %s resolves to %s: %s", ErrAddressRefused, host, addr.Unmap(), why)
		}
	}
	return nil
}

// guardedDial is the check at the last moment it can be made. The dialer has
// resolved the name; address is the IP and port it is about to connect to,
// and what is refused here is never connected to. There is no second
// resolution between this check and the connection.
func guardedDial(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" {
		return fmt.Errorf("%w: %s is not a TCP connection", ErrAddressRefused, network)
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s is not an address", ErrAddressRefused, address)
	}
	if why := refusedIP(ap.Addr()); why != "" {
		return fmt.Errorf("%w: %s: %s", ErrAddressRefused, ap.Addr().Unmap(), why)
	}
	if ap.Port() != 443 {
		return fmt.Errorf("%w: port %d is not the https port", ErrAddressRefused, ap.Port())
	}
	return nil
}

// guardedTransport refuses a request whose address is not a catalogue's
// before anything is resolved, and connects only to what guardedDial admits.
type guardedTransport struct {
	next http.RoundTripper
}

func (g guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := CheckAddress(req.URL.Scheme + "://" + req.URL.Host + "/"); err != nil {
		return nil, err
	}
	return g.next.RoundTrip(req)
}

// guardedClient is the only client a catalogue is fetched with outside a
// test.
func guardedClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   guardedDial,
	}
	transport := &http.Transport{
		// No proxy, whatever the environment says. A proxy would be dialled
		// instead of the catalogue, at an address nobody checked, and would
		// then resolve the catalogue's name itself.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       60 * time.Second,
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: guardedTransport{next: transport},
		// A catalogue redirecting somewhere else is a catalogue changing
		// which host serves the bytes -- and the way a public address hands a
		// request to a private one. It is not followed.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
