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

package gitops

import (
	"context"
	"strings"
)

// Where a cluster's profiles may come from (AD-14).
//
// On the Cluster CLAIM, not in the director's environment. The difference is
// not cosmetic: a source is where software enters this cluster, and opening
// one to a tenant is the platform administrator's act. On the claim it is a
// commit somebody reviewed, with an author and a date, next to everything
// else the cluster is. In an environment variable it is a deployment setting
// that changed when somebody rolled the Deployment, and the only record is
// whatever the pod spec happens to say now.
//
// It also makes the answer to "where could this cluster install software
// from" readable without cluster access, which is the question an audit asks
// and the one an environment variable answers worst.

// CatalogueSource is one repository of profile bundles.
type CatalogueSource struct {
	// Name is the catalogue's slug: the first half of a coordinate, so
	// "main/nextcloud-base-ce" is served by the source named main.
	Name string `json:"name"`
	// URL is where bundles are fetched from.
	URL string `json:"url"`
	// Access is "entitled" or "open".
	//
	// entitled: the store's own catalogue. An entry needs a signed grant
	// before a tenant may install it.
	//
	// open: a platform administrator's own repository. Entries install
	// without a statement from the store — but only for the tenants named
	// below, and nothing is open by default.
	Access string `json:"access,omitempty"`
	// Tenants may install from an open source. Empty means none, which is
	// what makes "open" mean "open to somebody" rather than "open to all".
	Tenants []string `json:"tenants,omitempty"`
}

// Open reports whether this source admits a tenant without a statement from
// the store.
func (s CatalogueSource) Open(tenant string) bool {
	if !strings.EqualFold(strings.TrimSpace(s.Access), "open") {
		return false
	}
	for _, t := range s.Tenants {
		if t == tenant {
			return true
		}
	}
	return false
}

// CatalogueSources reads the sources this cluster declares.
//
// A source with no name or no https URL is skipped rather than refused: the
// claim is read on every start, and one malformed entry must not stop a
// director from serving. What it costs is that installs from that catalogue
// are refused for want of a source, which says the same thing where somebody
// will see it.
func (g *GitOps) CatalogueSources(ctx context.Context) ([]CatalogueSource, error) {
	var claim struct {
		Spec struct {
			Catalogue struct {
				Sources []CatalogueSource `json:"sources"`
			} `json:"catalogue"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		return nil, err
	}
	out := make([]CatalogueSource, 0, len(claim.Spec.Catalogue.Sources))
	for _, s := range claim.Spec.Catalogue.Sources {
		s.Name = strings.TrimSpace(s.Name)
		s.URL = strings.TrimSpace(s.URL)
		if s.Name == "" || !strings.HasPrefix(s.URL, "https://") {
			// http:// is skipped here as it is everywhere else: the digest
			// makes the bytes safe, but a cluster fetching its catalogue in
			// clear announces what it runs.
			continue
		}
		out = append(out, s)
	}
	return out, nil
}
