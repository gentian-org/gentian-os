/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/hostnames"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// An app may not take an address name the platform depends on.
//
// The operator refuses it where the component would be published (the
// component is held, HostReserved). That is where it is enforced; this is
// where the person installing learns of it: asked before anything is
// committed, of the same list by the same function (internal/hostnames), so
// that the answer is a 422 with the reason and not an app that never
// appears.
//
// The profile is known at every install. One named with a coordinate is the
// bundle that was just fetched; one named alone is in the cluster's
// catalogue directory, which is what the cluster was given.

// reservedAddressZone is what the list has to know of where a tenant's apps
// are published. On a single-tenancy cluster the user tenant's hosts are
// directly under the cluster's domain, or fall back there the day a domain
// of its own is unbound, so the kernel's own names are refused to it; the
// platform tenant takes no apps and is refused before this is asked.
func (s *Server) reservedAddressZone(ctx context.Context) (hostnames.Zone, error) {
	settings, err := s.cfg.Repo.ClusterSettingValues(ctx)
	if err != nil && !errors.Is(err, gitops.ErrNoClusterClaim) {
		return hostnames.Zone{}, err
	}
	single := gentianov1alpha1.NormalizeTenancyMode(settings["tenancyMode"]) == gentianov1alpha1.TenancyModeSingle
	return hostnames.Zone{OnClusterDomain: single}, nil
}

// fetchedTakesReservedAddress is why a bundle fetched from a catalogue may
// not be installed under name, or "".
//
// The profile is asked as the cluster will hold it: with the origin the
// director records on everything it materialises. So nothing a catalogue
// serves is ever the platform's own component, whatever it is called and
// whatever trust tier it states.
func fetchedTakesReservedAddress(definition *gentianov1alpha1.ComponentProfile, origin string, zone hostnames.Zone) string {
	if definition == nil {
		// Fetch reads it or refuses the bundle; a bundle with no definition
		// is one nothing can be said of.
		return "its profile could not be read, so the addresses it would take are not known"
	}
	asked := definition.DeepCopy()
	if asked.Annotations == nil {
		asked.Annotations = map[string]string{}
	}
	asked.Annotations[profilebundle.OriginAnnotation] = origin
	if refusal := hostnames.Check(asked.Name, asked, zone); refusal != nil {
		return refusal.Message()
	}
	return ""
}

// onClusterTakesReservedAddress is why a profile named with no build may not
// be installed, or "": read from the cluster's catalogue directory.
func (s *Server) onClusterTakesReservedAddress(ctx context.Context, name string, zone hostnames.Zone) (string, error) {
	definition, err := s.cfg.Repo.ProfileDefinition(ctx, name)
	if errors.Is(err, gitops.ErrProfileUnreadable) {
		return "its profile on this cluster could not be read, so the addresses it would take are not known", nil
	}
	if err != nil {
		return "", err
	}
	if definition == nil {
		// Not in the directory: onClusterFor has answered that already.
		return "", nil
	}
	if refusal := hostnames.Check(name, definition, zone); refusal != nil {
		return refusal.Message(), nil
	}
	return "", nil
}

// refuseReservedAddress writes the refusal of an app or an add-on that would
// answer on a reserved address name. what names it: "app x", "add-on y".
func (s *Server) refuseReservedAddress(w http.ResponseWriter, r *http.Request, tenant, what, why string) {
	s.cfg.Log.WarnContext(r.Context(), "an install was refused for an address name the platform keeps",
		"request_id", reqID(r.Context()), "tenant", tenant, "what", what, "reason", why)
	s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
		"%s cannot be installed: %s. Nothing was committed or installed; "+
			"whoever publishes the profile has to change it", what, why))
}
