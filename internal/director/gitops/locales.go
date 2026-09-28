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
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// A tenant's languages, as a commit.
//
// Which languages this tenant's people read the platform in is an
// administrator's choice, so it is declared state and it travels the way every
// other choice does: the console asks the director, the director commits, Argo
// applies, and the thing that writes the realm is the composition that owns it
// (AD-15, and the same shape as the realm's security policy beside it).
//
// Nothing here holds a Keycloak credential, and a realm rebuilt from scratch
// comes back offering the same languages, because the answer is in git rather
// than in the realm.

// LocalesFile is the patch a tenant's languages are written to.
const LocalesFile = "locales.yaml"

// language is ISO 639-1, optionally with a region. Checked here as well as by
// the CRD so the refusal names the value rather than arriving as a rejected
// commit somebody has to go and find.
var languagePattern = regexp.MustCompile(`^[a-z]{2}(-[A-Za-z0-9]{2,8})?$`)

// TenantLocales reads a tenant's languages back, so a screen shows what it is
// about to change rather than a blank.
func (g *GitOps) TenantLocales(ctx context.Context, tenant string) ([]string, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), LocalesFile))
	if errors.Is(err, os.ErrNotExist) {
		// Nothing declared, which is a real answer: the realm offers the
		// platform's own set.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			Locales []string `json:"locales"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Spec.Locales, nil
}

// SetTenantLocales commits the languages this tenant's realm offers.
//
// An empty list clears the declaration rather than declaring no languages: a
// realm offering nothing would be a login page nobody could read, so the
// absence of the file is what "the platform's own set" is spelled as.
func (g *GitOps) SetTenantLocales(ctx context.Context, tenant string, locales []string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	clean := make([]string, 0, len(locales))
	seen := map[string]struct{}{}
	for _, lang := range locales {
		lang = strings.TrimSpace(lang)
		if lang == "" {
			continue
		}
		if !languagePattern.MatchString(lang) {
			return Result{}, fmt.Errorf("%q is not a language code", lang)
		}
		if _, dup := seen[lang]; dup {
			continue
		}
		seen[lang] = struct{}{}
		clean = append(clean, lang)
	}
	body := ""
	if len(clean) > 0 {
		body = renderLocales(tenant, clean)
	}
	return g.writeTenantFile(ctx, tenant, LocalesFile, body, listPatch,
		fmt.Sprintf("Set the languages for tenant %s", tenant), meta)
}

// renderLocales writes the patch a reviewer reads in the commit.
func renderLocales(tenant string, locales []string) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: the languages this tenant's realm offers on\n")
	b.WriteString("# its login and account pages, set in the console by whoever the commit\n")
	b.WriteString("# names. The composition turns them into the realm's own fields, so\n")
	b.WriteString("# nothing holds a Keycloak credential to apply them and a realm rebuilt\n")
	b.WriteString("# from scratch comes back offering the same ones.\n")
	b.WriteString("#\n")
	b.WriteString("# Keycloak ships the translations; this only says which to offer. A\n")
	b.WriteString("# language the desktop has no catalogue for still logs in translated and\n")
	b.WriteString("# then shows an English desktop.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  locales:\n")
	for _, lang := range locales {
		b.WriteString("    - " + lang + "\n")
	}
	return b.String()
}
