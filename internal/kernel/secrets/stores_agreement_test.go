/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// Which store an ExternalSecret names is what keeps a kernel secret out of a
// tenant's namespace, and it is said in four places that nothing else holds
// together: the Compositions, the operator, the installer's copy of the
// kernel's store, and the names in this package. These tests read what the
// Compositions render (the golden files of crossplane/tests/unit/render) and
// hold all of it to one rule:
//
//   - an ExternalSecret in a tenant's namespace names that tenant's store,
//   - an ExternalSecret in a kernel or system namespace names the kernel's,
//   - the kernel's store admits the kernel and system tiers and the platform
//     tenant's namespace, a tenant's store its own namespace only,
//   - a tenant's store signs in with a role whose policy grants nothing
//     outside that tenant's paths.
//
// What the policies mean to OpenBao is asserted against a running one by
// scripts/tools/verify-openbao-policies.sh; here it is the text.

const repoRoot = "../../.."

type object = map[string]interface{}

// rendered returns every object a golden file holds, the manifests inside
// provider-kubernetes Objects included.
func rendered(t *testing.T) map[string][]object {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot, "crossplane/tests/unit/render/*/expected.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden files: %v", err)
	}
	out := map[string][]object{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
		for {
			doc := object{}
			if err := dec.Decode(&doc); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("%s: %v", file, err)
			}
			if len(doc) == 0 {
				continue
			}
			name := filepath.Base(filepath.Dir(file))
			out[name] = append(out[name], doc)
			if manifest, ok := dig(doc, "spec", "forProvider", "manifest").(object); ok {
				out[name] = append(out[name], manifest)
			}
		}
	}
	return out
}

func dig(o interface{}, path ...string) interface{} {
	for _, p := range path {
		m, ok := o.(object)
		if !ok {
			return nil
		}
		o = m[p]
	}
	return o
}

func str(o interface{}, path ...string) string {
	s, _ := dig(o, path...).(string)
	return s
}

// storeOf is the store an ExternalSecret spec reads through, "" when it reads
// through none (a generator's values only). It fails on a store named
// anywhere but in secretStoreRef: a dataFrom or data entry with a store of
// its own would pass by this test.
func storeOf(t *testing.T, where string, spec interface{}) string {
	t.Helper()
	for _, list := range []string{"data", "dataFrom"} {
		entries, _ := dig(spec, list).([]interface{})
		for _, e := range entries {
			if dig(e, "sourceRef", "storeRef") != nil {
				t.Errorf("%s: a %s entry names a store of its own", where, list)
			}
		}
	}
	ref, ok := dig(spec, "secretStoreRef").(object)
	if !ok {
		return ""
	}
	if kind := str(ref, "kind"); kind != secrets.StoreKind {
		t.Errorf("%s: secretStoreRef.kind = %q, want %s", where, kind, secrets.StoreKind)
	}
	return str(ref, "name")
}

// wantStore is the store an ExternalSecret in a namespace has to name.
func wantStore(t *testing.T, where, namespace string) string {
	t.Helper()
	switch {
	case strings.HasPrefix(namespace, "tenant-"):
		if strings.HasSuffix(namespace, "-dmz") {
			t.Errorf("%s: an ExternalSecret that reads OpenBao in a tenant's DMZ %s; no store admits it", where, namespace)
		}
		return secrets.TenantStore(strings.TrimPrefix(namespace, "tenant-"))
	case strings.HasPrefix(namespace, "kernel-"), strings.HasPrefix(namespace, "system-"):
		return secrets.KernelStore
	}
	t.Errorf("%s: namespace %q is of no tier this test knows", where, namespace)
	return ""
}

func TestEveryRenderedExternalSecretNamesTheStoreOfItsNamespace(t *testing.T) {
	seen := map[string]int{}
	for fixture, objects := range rendered(t) {
		for _, o := range objects {
			name := str(o, "metadata", "name")
			switch str(o, "kind") {
			case "ExternalSecret":
				if dig(o, "spec", "forProvider") != nil {
					continue
				}
				namespace := str(o, "metadata", "namespace")
				where := fmt.Sprintf("%s: ExternalSecret %s/%s", fixture, namespace, name)
				store := storeOf(t, where, dig(o, "spec"))
				if store == "" {
					continue
				}
				if want := wantStore(t, where, namespace); store != want {
					t.Errorf("%s reads through %q, want %q", where, store, want)
				}
				seen[strings.SplitN(namespace, "-", 2)[0]]++

			case "ClusterExternalSecret":
				if dig(o, "spec", "forProvider") != nil {
					continue
				}
				where := fmt.Sprintf("%s: ClusterExternalSecret %s", fixture, name)
				store := storeOf(t, where, dig(o, "spec", "externalSecretSpec"))
				tenants := map[string]bool{}
				selectors, _ := dig(o, "spec", "namespaceSelectors").([]interface{})
				if one := dig(o, "spec", "namespaceSelector"); one != nil {
					selectors = append(selectors, one)
				}
				for _, sel := range selectors {
					labels, _ := dig(sel, "matchLabels").(object)
					tenant, _ := labels[layout.LabelTenant].(string)
					tier, _ := labels[layout.LabelTier].(string)
					if tenant != "" && tier != string(layout.TierTenant) {
						t.Errorf("%s selects tenant %q without the tier, which also matches its DMZ", where, tenant)
					}
					tenants[tenant] = true
				}
				switch {
				case len(tenants) == 1 && !tenants[""]:
					for tenant := range tenants {
						if want := secrets.TenantStore(tenant); store != want {
							t.Errorf("%s reads through %q, want %q", where, store, want)
						}
					}
					seen["tenant-ces"]++
				case len(tenants) == 1:
					// No tenant named: the cluster's own credential, which
					// only the kernel's store reads.
					if store != secrets.KernelStore {
						t.Errorf("%s reads through %q, want %q", where, store, secrets.KernelStore)
					}
				default:
					t.Errorf("%s selects %d kinds of namespace; one store cannot serve them", where, len(tenants))
				}

			case "PushSecret", "SecretStore":
				t.Errorf("%s: a %s %q is rendered; nothing the platform makes is one", fixture, str(o, "kind"), name)
			}
		}
	}
	// The rule is only shown for what the fixtures render.
	for _, tier := range []string{"tenant", "kernel", "tenant-ces"} {
		if seen[tier] == 0 {
			t.Errorf("no golden file renders an ExternalSecret of kind %q; this test covers nothing there", tier)
		}
	}
}

// kernelStoreConditions is what the kernel's store has to say about who may
// name it.
func kernelStoreConditions() []interface{} {
	return []interface{}{
		object{"namespaceSelector": object{"matchExpressions": []interface{}{
			object{"key": layout.LabelTier, "operator": "In", "values": []interface{}{
				string(layout.TierKernel), string(layout.TierSystem),
			}},
		}}},
		object{"namespaces": []interface{}{layout.Tenant(gentianov1alpha1.PlatformTenantName)}},
	}
}

func TestTheKernelStoreAdmitsKernelAndSystemNamespacesOnly(t *testing.T) {
	want := kernelStoreConditions()
	found := 0
	for fixture, objects := range rendered(t) {
		for _, o := range objects {
			if str(o, "kind") != secrets.StoreKind || str(o, "metadata", "name") != secrets.KernelStore {
				continue
			}
			found++
			if got := dig(o, "spec", "conditions"); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: the kernel's store has conditions %v, want %v", fixture, got, want)
			}
			if role := str(o, "spec", "provider", "vault", "auth", "kubernetes", "role"); role != "eso" {
				t.Errorf("%s: the kernel's store signs in as %q", fixture, role)
			}
		}
	}
	if found == 0 {
		t.Fatal("no golden file renders the kernel's store")
	}

	// The installer's copy, applied when the object is not there yet.
	raw, err := os.ReadFile(filepath.Join(repoRoot, "kernel/services/_globals/eso-cluster-secret-store.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	static := object{}
	if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(&static); err != nil {
		t.Fatal(err)
	}
	if str(static, "metadata", "name") != secrets.KernelStore {
		t.Fatalf("the installer's copy is named %q", str(static, "metadata", "name"))
	}
	if got := dig(static, "spec", "conditions"); !reflect.DeepEqual(got, want) {
		t.Errorf("the installer's copy of the kernel's store has conditions %v, want %v", got, want)
	}
}

func TestATenantStoreIsItsTenantsAlone(t *testing.T) {
	policies := map[string]string{}
	roles := map[string]object{}
	stores := map[string]object{}
	for fixture, objects := range rendered(t) {
		if !strings.HasPrefix(fixture, "tenant-") {
			continue
		}
		for _, o := range objects {
			switch str(o, "kind") {
			case "Policy":
				policies[str(o, "spec", "forProvider", "name")] = str(o, "spec", "forProvider", "policy")
			case "AuthBackendRole":
				if fp, ok := dig(o, "spec", "forProvider").(object); ok {
					roles[str(fp, "roleName")] = fp
				}
			case secrets.StoreKind:
				stores[str(o, "metadata", "name")] = o
			}
		}
	}
	if len(stores) == 0 {
		t.Fatal("no tenant golden file renders a store")
	}
	for name, store := range stores {
		tenant := strings.TrimPrefix(name, "openbao-tenant-")
		if name != secrets.TenantStore(tenant) || tenant == "" {
			t.Errorf("store %q is not named as secrets.TenantStore names one", name)
			continue
		}
		wantConditions := []interface{}{object{"namespaceSelector": object{"matchLabels": object{
			layout.LabelTier:   string(layout.TierTenant),
			layout.LabelTenant: tenant,
		}}}}
		if got := dig(store, "spec", "conditions"); !reflect.DeepEqual(got, wantConditions) {
			t.Errorf("store %s has conditions %v, want %v", name, got, wantConditions)
		}

		roleName := str(store, "spec", "provider", "vault", "auth", "kubernetes", "role")
		if roleName != "eso-tenant-"+tenant {
			t.Errorf("store %s signs in as %q", name, roleName)
		}
		role, ok := roles[roleName]
		if !ok {
			t.Errorf("store %s signs in as %q, which the tenant's Composition does not make", name, roleName)
			continue
		}
		if got := role["tokenPolicies"]; !reflect.DeepEqual(got, []interface{}{"eso-tenant-" + tenant}) {
			t.Errorf("role %s carries the policies %v, want its tenant's one only", roleName, got)
		}
		// Bound to ESO's own ServiceAccount, the one the store signs in
		// with: nothing in the tenant's namespace can use the role.
		sa := dig(store, "spec", "provider", "vault", "auth", "kubernetes", "serviceAccountRef")
		if !reflect.DeepEqual(role["boundServiceAccountNames"], []interface{}{str(sa, "name")}) ||
			!reflect.DeepEqual(role["boundServiceAccountNamespaces"], []interface{}{str(sa, "namespace")}) {
			t.Errorf("role %s is bound to %v in %v, the store signs in as %v",
				roleName, role["boundServiceAccountNames"], role["boundServiceAccountNamespaces"], sa)
		}
		if ns := str(sa, "namespace"); strings.HasPrefix(ns, "tenant-") || ns == "" {
			t.Errorf("role %s is bound to a ServiceAccount in %q", roleName, ns)
		}

		policy, ok := policies["eso-tenant-"+tenant]
		if !ok {
			t.Errorf("the policy eso-tenant-%s is not rendered", tenant)
			continue
		}
		own := "/gentian-os/tenants/" + tenant + "/"
		granted := 0
		for _, stanza := range strings.Split(policy, "path ")[1:] {
			path := strings.Trim(strings.SplitN(stanza, "{", 2)[0], " \"\n")
			if strings.Contains(stanza, `"deny"`) {
				continue
			}
			granted++
			if !strings.Contains(path, own) || strings.ContainsAny(strings.SplitN(path, own, 2)[0], "*+") {
				t.Errorf("policy eso-tenant-%s grants %q, outside the tenant's own paths", tenant, path)
			}
			if !strings.Contains(stanza, `capabilities = ["read"]`) {
				t.Errorf("policy eso-tenant-%s grants more than read on %q", tenant, path)
			}
		}
		if granted == 0 {
			t.Errorf("policy eso-tenant-%s grants nothing", tenant)
		}
		for _, denied := range []string{"/data/gentian-os/kernel/*", "/metadata/gentian-os/kernel/*"} {
			if !strings.Contains(policy, denied+`" {`+"\n  capabilities = [\"deny\"]") {
				t.Errorf("policy eso-tenant-%s does not deny %s", tenant, denied)
			}
		}
	}
}

// The kernel's own charts are templates and are not rendered here. None of
// them is installed into a tenant's namespace, and none may name a tenant's
// store: its name is made from a tenant's, which a kernel chart does not have.
func TestNoKernelChartNamesATenantStore(t *testing.T) {
	prefix := strings.TrimSuffix(secrets.TenantStore("x"), "x")
	for _, dir := range []string{"kernel", "charts"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".tpl") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(raw, []byte(prefix)) {
				t.Errorf("%s names a tenant's store (%s…)", path, prefix)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
