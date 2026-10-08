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
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

// Whether this cluster offers an App Store is one answer, given in one place.
//
// Two things have to hold: the cluster reports what it runs (an app installed
// through a store is what the licence report lists), and the Cluster claim
// names a store (spec.catalogue.storeUrl). Three readers follow from it and
// must not disagree: the tenant reconciler, which places a Component of every
// profile declaring defaultWhereStoreOffered and takes it away again; the
// component reconciler, which tells such a component where the store is and
// opens its way there; and the tile projection, which writes the verdict
// beside the tiles for the usher to hand to whoever asks.
//
// The first half is the operator's own setting (LICENCE_REPORT_ENABLED, as the
// chart renders it) and the second is read from the claim itself, where the
// director reads it from too.

// storeOffer is the verdict: where the store is, or why none is offered.
type storeOffer struct {
	// url is the base address of the store's API. Empty unless offered.
	url string
	// reason is why no store is offered, in the words the usher answers
	// with. Empty when one is.
	reason string
}

func (o storeOffer) offered() bool { return o.reason == "" && o.url != "" }

// storeAddress is what a store's address has to look like to be used: https,
// a host, an optional path, no query, no fragment and no trailing slash. It is
// the pattern the App Store app's chart holds store.url to, so an address that
// passes here is one the release will not be refused for.
var storeAddress = regexp.MustCompile(`^https://[^\s/?#]+(/[^\s?#]*[^\s/?#])?$`)

// normaliseStoreAddress is the claim's storeUrl as a component is told it, or
// "" for one that cannot be used. A trailing slash is dropped rather than
// refused: it is the one difference people write without meaning anything.
func normaliseStoreAddress(raw string) string {
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !storeAddress.MatchString(v) {
		return ""
	}
	return v
}

// readStoreOffer answers whether the cluster offers an App Store.
//
// An error is an error and not "no store": a claim that could not be read
// says nothing, and the callers that would take something away on a "no"
// leave things as they are instead.
func readStoreOffer(ctx context.Context, c client.Reader, licenceReporting bool) (storeOffer, error) {
	if !licenceReporting {
		// Said without reading the claim: with reporting off there is no
		// store whatever the claim names.
		return storeOffer{reason: tilecatalogue.AppStoreReasonNoLicenceReport}, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: clusterClaimGVK.Group, Version: clusterClaimGVK.Version, Kind: clusterClaimGVK.Kind + "List",
	})
	if err := c.List(ctx, list, client.InNamespace(clusterConfigNamespace)); err != nil {
		return storeOffer{}, err
	}
	// A cluster has one claim. Sorted all the same, so that a second one
	// somebody applied does not make the answer depend on list order.
	sort.Slice(list.Items, func(a, b int) bool { return list.Items[a].GetName() < list.Items[b].GetName() })
	for i := range list.Items {
		raw, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "catalogue", "storeUrl")
		if url := normaliseStoreAddress(raw); url != "" {
			return storeOffer{url: url}, nil
		}
	}
	return storeOffer{reason: tilecatalogue.AppStoreReasonNoStore}, nil
}

// wantsStore reports whether a profile asked to be told where the store is.
// Like the director and the usher it is a fact that changes what the
// component may reach, and the only one that leads outside the cluster.
func wantsStore(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).StoreURLKey != "" &&
		profile.Spec.TrustTier == gentianov1alpha1.TrustTierPlatform
}

// storeEgressExcept are the addresses a component told where the store is may
// NOT reach on 443, although the rule names every address: everything that is
// not a public IPv4 unicast address. That takes out the pod, service and node
// ranges of a cluster (all private or carrier-grade NAT space), the loopback,
// the link-local range the cloud metadata address is in (169.254.169.254),
// and multicast and reserved space.
var storeEgressExcept = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"224.0.0.0/3",
}

// storeEgressRule is the way out of the cluster for a component that talks to
// a store: TCP 443 to public IPv4 addresses, and nothing else.
//
// Not the store's own address. A store names two further kinds of address in
// its own metadata -- its issuer and the origins its pictures are served from
// -- which are known only once it has been asked, and a NetworkPolicy matches
// addresses, not names; the platform writes no policy kind that matches names.
// So this is the narrowest rule that still works: the port a store is served
// on, to addresses that are not this cluster's and not anybody's private
// network. Which host is asked is the component's to restrict, and it does.
//
// No IPv6 block is named, so nothing is admitted over IPv6. DNS is the
// namespace baseline's.
func storeEgressRule() networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	https := intstr.FromInt32(443)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: append([]string(nil), storeEgressExcept...)},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &https}},
	}
}

// storeEgress is the store rule for a component whose profile asked where the
// store is, while the cluster offers one. No store, no way out.
func (r *ComponentReconciler) storeEgress(ctx context.Context, profile *gentianov1alpha1.ComponentProfile) ([]networkingv1.NetworkPolicyEgressRule, error) {
	if !wantsStore(profile) {
		return nil, nil
	}
	offer, err := readStoreOffer(ctx, r.Client, r.LicenceReporting)
	if err != nil {
		return nil, err
	}
	if !offer.offered() {
		return nil, nil
	}
	return []networkingv1.NetworkPolicyEgressRule{storeEgressRule()}, nil
}

// placementValues are the two platform facts that are not the same for every
// component of a zone: the host this component answers on, and where the
// store is. Each lands where the profile's valueMapping.platform says, and a
// profile that names neither key receives nothing and reads no claim.
func (r *ComponentReconciler) placementValues(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, zone edgeZone) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	m := platformMapping(profile)
	if m == nil {
		return out, nil
	}
	if m.HostKey != "" {
		setPath(out, m.HostKey, componentOwnHost(comp, profile, zone.zoneNames))
	}
	if m.StoreURLKey != "" {
		url := ""
		if wantsStore(profile) {
			offer, err := readStoreOffer(ctx, r.Client, r.LicenceReporting)
			if err != nil {
				return nil, err
			}
			url = offer.url
		}
		setPath(out, m.StoreURLKey, url)
	}
	return out, nil
}

// componentOwnHost is where a component's first own gateway entry answers in
// its zone, or "" when it has none.
func componentOwnHost(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, zone zoneNames) string {
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.Surface != gentianov1alpha1.SurfaceGateway {
			continue
		}
		if e.Backend.Component != "" && e.Backend.Component != comp.Name {
			continue
		}
		return exposureHostIn(zone, comp.Name, e)
	}
	return ""
}

// clusterClaimObject is the Cluster claim as something to watch.
func clusterClaimObject() *unstructured.Unstructured {
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(clusterClaimGVK)
	return claim
}

// componentsToldTheStore re-runs every Component whose profile asked where
// the store is when the claim changes: the address is one of its values, and
// nothing else would carry a new one to the release.
func componentsToldTheStore(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		profiles := &gentianov1alpha1.ComponentProfileList{}
		if err := c.List(ctx, profiles); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range profiles.Items {
			if wantsStore(&profiles.Items[i]) {
				out = append(out, componentsReferencingProfile(ctx, c, profiles.Items[i].Name)...)
			}
		}
		return out
	})
}
