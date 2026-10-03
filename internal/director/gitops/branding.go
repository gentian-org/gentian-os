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
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/branding"
)

// BrandingFile is the cluster's Branding, among its other declarations.
const BrandingFile = "branding.yaml"

// ErrInvalidBranding is a brand the pages could not show.
var ErrInvalidBranding = errors.New("invalid branding")

// ClusterBranding reads the cluster's brand; false when it sets none and the
// pages show the platform's own.
func (g *GitOps) ClusterBranding(ctx context.Context) (*gentianov1alpha1.BrandingSpec, bool, error) {
	var doc gentianov1alpha1.Branding
	found, err := g.readClaimsFile(ctx, BrandingFile, &doc)
	if err != nil || !found {
		return nil, found, err
	}
	return &doc.Spec, true, nil
}

// SetClusterBranding commits the cluster's brand. It is rendered first, the
// way the operator will render it, so a brand the pages could not show is
// refused here rather than reported on a status nobody is reading.
func (g *GitOps) SetClusterBranding(ctx context.Context, spec gentianov1alpha1.BrandingSpec, meta Meta) (Result, error) {
	if _, err := branding.Render(&spec); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidBranding, err)
	}
	doc := gentianov1alpha1.Branding{Spec: spec}
	doc.APIVersion = gentianov1alpha1.GroupVersion.String()
	doc.Kind = "Branding"
	doc.Name = gentianov1alpha1.BrandingName
	body, err := yaml.Marshal(doc)
	if err != nil {
		return Result{}, err
	}
	header := "# Managed by the director: the brand every page on this cluster shows,\n" +
		"# set by whoever the commit names. tokens is a W3C design-token document.\n"
	return g.writeClaimsFile(ctx, BrandingFile, header+stripEmptyStatus(string(body)), "Set the cluster's branding", meta)
}

// stripEmptyStatus drops the status and creationTimestamp a marshalled
// object carries, which belong to the cluster and not to git.
func stripEmptyStatus(text string) string {
	var out strings.Builder
	inStatus := false
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		switch {
		case line == "status: {}" || line == "  creationTimestamp: null":
			continue
		case line == "status:":
			inStatus = true
			continue
		case inStatus && strings.HasPrefix(line, " "):
			continue
		}
		inStatus = false
		out.WriteString(line + "\n")
	}
	return out.String()
}
