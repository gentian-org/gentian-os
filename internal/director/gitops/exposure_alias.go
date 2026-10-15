/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/hostnames"
)

// An alias is a second address for one published surface, directly under the
// cluster's own domain: statistics.<cluster> for a surface that answers at
// statistics.<tenant>.<cluster>. It is the cluster administrator's to give,
// because a name under the cluster's domain is the cluster's, and it is
// recorded on the surface's own entry, so that withdrawing the surface takes
// the alias with it and nothing is ever published on the strength of an alias
// alone.

// ErrAliasInvalid is an alias that cannot be one: not a single name directly
// under the cluster's domain, a name the platform keeps, or asked on a
// cluster where it means nothing.
var ErrAliasInvalid = errors.New("invalid alias")

// ErrAliasHeld is an alias whose name is in use: a tenant is called it, or
// another surface answers there.
var ErrAliasHeld = errors.New("the address is already in use")

// ErrNotPublished is an alias asked for a surface the tenant's perimeter
// approver has not published.
var ErrNotPublished = errors.New("the surface is not published")

// ErrNameIsAlias is a tenant asked for under a name a surface answers at.
var ErrNameIsAlias = errors.New("the name is the address of a published surface")

var aliasLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// AliasLabel is the one label of host directly under kernel, or the reason
// host cannot be an alias.
func AliasLabel(host, kernel string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	label, found := strings.CutSuffix(host, "."+kernel)
	if !found || label == "" || len(label) > 63 || !aliasLabelPattern.MatchString(label) {
		return "", fmt.Errorf("%w: %q is not a name directly under %s. An alias is one name there, like statistics.%s",
			ErrAliasInvalid, host, kernel, kernel)
	}
	if kept := hostnames.Kept(label); kept != nil {
		return "", fmt.Errorf("%w: %s is kept for the platform (%s) and no surface is given it",
			ErrAliasInvalid, host, kept.Why)
	}
	return label, nil
}

// heldAlias is where a label is in use as an alias.
type heldAlias struct {
	tenant string
	key    string
}

// aliasesHeld reads every alias git declares, by label. dir is the cluster's
// tenants directory; the caller has the repository current.
func aliasesHeld(dir string) (map[string]heldAlias, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	held := map[string]heldAlias{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name(), ExposuresFile))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var doc struct {
			Spec struct {
				Exposures []Exposure `json:"exposures"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("tenant %s: %s does not parse: %w", entry.Name(), ExposuresFile, err)
		}
		for _, e := range doc.Spec.Exposures {
			if e.Alias != "" {
				held[e.Alias] = heldAlias{tenant: entry.Name(), key: e.Key()}
			}
		}
	}
	return held, nil
}

// SetExposureAlias gives one published surface a second address directly
// under the cluster's domain, or takes it away when host is empty.
//
// Refused before anything is written: a host that is not one name under the
// cluster's domain, or is a name the platform keeps (ErrAliasInvalid); a
// single-tenancy cluster, where the user tenant's addresses are under the
// cluster's domain already (ErrAliasInvalid); a name a tenant is called, or
// one another surface holds (ErrAliasHeld); and a surface the tenant's
// perimeter approver has not published, has let expire, or that is not a
// public address at all (ErrNotPublished). The alias follows the surface:
// it is kept through the approver's reviews and goes when the surface is
// withdrawn.
func (g *GitOps) SetExposureAlias(ctx context.Context, tenant, install, exposure, host string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !ValidName(install) {
		return Result{}, fmt.Errorf("%w: install %q", ErrInvalidName, install)
	}
	if !ValidName(exposure) {
		return Result{}, fmt.Errorf("%w: exposure %q", ErrInvalidName, exposure)
	}
	label := ""
	if strings.TrimSpace(host) != "" {
		kernel, err := g.KernelDomain(ctx)
		if err != nil {
			return Result{}, err
		}
		if label, err = AliasLabel(host, kernel); err != nil {
			return Result{}, err
		}
		settings, err := g.ClusterSettingValues(ctx)
		if err != nil && !errors.Is(err, ErrNoClusterClaim) {
			return Result{}, err
		}
		if gentianov1alpha1.NormalizeTenancyMode(settings["tenancyMode"]) == gentianov1alpha1.TenancyModeSingle {
			return Result{}, fmt.Errorf("%w: on a single-tenancy cluster the user tenant's addresses are directly under %s already",
				ErrAliasInvalid, kernel)
		}
		host = label + "." + kernel
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	have, err := g.tenantExposures(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	want := Exposure{Install: install, ExposureName: exposure}.Key()
	at := -1
	for i := range have {
		if have[i].Key() == want {
			at = i
		}
	}
	what := install + "/" + exposure
	if label == "" {
		if at < 0 || have[at].Alias == "" {
			return Result{Status: "unchanged"}, nil
		}
		have[at].Alias, have[at].AliasSetBy, have[at].AliasSetAt = "", "", ""
		return g.writeTenantFileLocked(ctx, tenant, ExposuresFile, renderExposures(tenant, have), listPatch,
			fmt.Sprintf("No alias for %s of tenant %s", what, tenant), meta)
	}

	switch {
	case at < 0:
		return Result{}, fmt.Errorf("%w: %s of tenant %s. Its perimeter approver publishes it first", ErrNotPublished, what, tenant)
	case !have[at].live(time.Now()):
		return Result{}, fmt.Errorf("%w: the approval of %s of tenant %s has expired", ErrNotPublished, what, tenant)
	case have[at].Kind == KindSignInAppAuthorization:
		return Result{}, fmt.Errorf("%w: %s of tenant %s is behind sign-in and has no public address", ErrNotPublished, what, tenant)
	case have[at].Apex:
		return Result{}, fmt.Errorf("%w: %s of tenant %s is on the cluster's main address", ErrAliasInvalid, what, tenant)
	}

	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	tenants := filepath.Dir(filepath.Dir(manifest))
	if _, err := os.Stat(filepath.Join(tenants, label, "tenant.yaml")); err == nil {
		return Result{}, fmt.Errorf("%w: %s is tenant %s's own address", ErrAliasHeld, host, label)
	}
	held, err := aliasesHeld(tenants)
	if err != nil {
		return Result{}, err
	}
	if h, taken := held[label]; taken && (h.tenant != tenant || h.key != want) {
		return Result{}, fmt.Errorf("%w: %s is the alias of %s of tenant %s",
			ErrAliasHeld, host, strings.Replace(h.key, " ", "/", 1), h.tenant)
	}
	if have[at].Alias == label {
		return Result{Status: "unchanged"}, nil
	}
	have[at].Alias = label
	have[at].AliasSetBy = meta.Subject
	have[at].AliasSetAt = time.Now().UTC().Format(time.RFC3339)
	return g.writeTenantFileLocked(ctx, tenant, ExposuresFile, renderExposures(tenant, have), listPatch,
		fmt.Sprintf("Alias %s for %s of tenant %s", host, what, tenant), meta)
}
