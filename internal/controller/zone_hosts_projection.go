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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A zone's Keycloak client admits a redirect only to a host it lists, and the
// list was written by hand in the tenant Composition: console, admin, and for
// the kernel realm argocd, headlamp and id. A component whose profile declares
// any other subDomain got "Invalid parameter: redirect_uri" on an error page
// that names neither the component nor a redirect URI list. The administration
// console hit exactly that the first time its tile was opened.
//
// The operator is what knows every host a zone serves, because it composes the
// routes. This projects that per tenant, the way the tile catalogue is already
// projected, so the list follows the components.
//
// What it does NOT cover, and cannot: argocd, headlamp and id are kernel tier.
// They are installed by install.sh and are not components, so nothing here
// sees them and the Composition still names them. Two of the five hosts follow
// the components; three are still a list, for a reason rather than an
// oversight.
const (
	// zoneHostsConfigType labels the ConfigMaps this writes, so the
	// Composition's extra-resources step can select them by kind rather than
	// by guessing a name.
	zoneHostsConfigType = "zone-hosts"
	// zoneHostsKey is the one key: a sorted, newline-separated list of host
	// labels, which is what a Composition can read without parsing.
	zoneHostsKey = "hosts"
)

// zoneHostsConfigMapName is one ConfigMap per tenant, in the control
// namespace beside the tile catalogue rather than in the tenant's own, so a
// tenant that has not been provisioned yet still has somewhere for this to
// land.
func zoneHostsConfigMapName(tenant string) string { return "gentian-zone-hosts-" + tenant }

// projectZoneHosts writes, for every tenant, the host labels its components'
// gateway exposures serve.
//
// Labels and not full hostnames: the Composition builds the redirect URI from
// the tenant's effective domain, which it knows and this does not -- a tenant
// with a custom domain is served at a different suffix than the one the
// operator's KernelDomain would produce.
func (r *TileProjectionReconciler) projectZoneHosts(ctx context.Context) error {
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return err
	}
	components := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, components); err != nil {
		return err
	}

	byNamespace := map[string]string{}
	for i := range tenants.Items {
		byNamespace[tenants.Items[i].NamespaceName()] = tenants.Items[i].Name
	}

	// Every tenant gets an entry, including one whose components declare no
	// host of their own: an empty projection is the answer "these components
	// need nothing added", and its absence would be indistinguishable from
	// "the operator has not looked yet".
	hosts := map[string]map[string]bool{}
	for i := range tenants.Items {
		hosts[tenants.Items[i].Name] = map[string]bool{}
	}

	for i := range components.Items {
		comp := &components.Items[i]
		if comp.DeletionTimestamp != nil {
			continue
		}
		tenant, ok := byNamespace[comp.Namespace]
		if !ok {
			// A service or a shared backend is in nobody's zone.
			continue
		}
		profile := &gentianov1alpha1.ComponentProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: comp.Spec.ProfileRef.Name}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		for j := range profile.Spec.Expose {
			e := &profile.Spec.Expose[j]
			// Gateway entries only. A perimeter surface is published through
			// its own proxy with its own credential and never reaches the
			// zone's client, so listing it as a redirect URI would widen the
			// client for a host the zone does not serve.
			if e.Surface != gentianov1alpha1.SurfaceGateway {
				continue
			}
			label := e.SubDomain
			if label == "" {
				// An entry with no subDomain is served on the component's own
				// name, which is what the route builder uses.
				label = comp.Name
			}
			hosts[tenant][label] = true
		}
	}

	for tenant, set := range hosts {
		if err := r.writeZoneHosts(ctx, tenant, sortedKeys(set)); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeZoneHosts creates the ConfigMap the first time and patches it only
// when the content differs, so a projection that did not change does not wake
// every Composition that reads it.
func (r *TileProjectionReconciler) writeZoneHosts(ctx context.Context, tenant string, hosts []string) error {
	key := types.NamespacedName{
		Name:      zoneHostsConfigMapName(tenant),
		Namespace: layout.Namespace(layout.Control),
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				managedByLabel:             managedByValue,
				"gentianos.io/config-type": zoneHostsConfigType,
				tenantLabel:                tenant,
			},
		},
		Data: map[string]string{zoneHostsKey: strings.Join(hosts, "\n")},
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, key, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Data, desired.Data) &&
		equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	return r.Patch(ctx, existing, patch)
}
