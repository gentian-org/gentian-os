/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package kernelnet

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
)

const (
	repoRoot     = "../../.."
	manifestPath = "kernel/security/network-policies/kernel-network-policies.yaml"
)

func load(t *testing.T) *Inventory {
	t.Helper()
	inv, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// What the installer applies is what the inventory says, to the byte.
func TestTheManifestIsTheInventorys(t *testing.T) {
	want, err := load(t).Manifest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot, manifestPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not what the inventory generates: run `make gen-kernel-network-policies`", manifestPath)
	}
}

// Every kernel namespace has one policy that selects every pod, which is
// what refuses an ingress nothing lists. No policy says anything of egress,
// none is in a namespace that is not a kernel one, and the mail namespaces
// in particular carry none of these.
func TestEveryKernelNamespaceRefusesByDefault(t *testing.T) {
	inv := load(t)
	policies := inv.Policies()
	kernel := map[string]bool{}
	for _, ns := range layout.KernelNamespaces() {
		kernel[ns] = false
	}
	for _, p := range policies {
		if _, ok := kernel[p.Namespace]; !ok {
			t.Errorf("%s/%s: not a kernel namespace", p.Namespace, p.Name)
		}
		if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
			t.Errorf("%s/%s: policyTypes %v; these rules are ingress only", p.Namespace, p.Name, p.Spec.PolicyTypes)
		}
		if len(p.Spec.Egress) != 0 {
			t.Errorf("%s/%s restricts egress", p.Namespace, p.Name)
		}
		if p.Labels[PolicyLabel] != "true" {
			t.Errorf("%s/%s lacks the label the installer removes it by", p.Namespace, p.Name)
		}
		for _, rule := range p.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.IPBlock != nil {
					t.Errorf("%s/%s names an address block; node and API server addresses differ per cluster", p.Namespace, p.Name)
				}
			}
		}
		if p.Name == NamespacePolicyName && len(p.Spec.PodSelector.MatchLabels) == 0 && len(p.Spec.PodSelector.MatchExpressions) == 0 {
			kernel[p.Namespace] = true
		}
	}
	for ns, has := range kernel {
		if !has {
			t.Errorf("%s has no policy selecting every pod", ns)
		}
	}
	for _, e := range inv.Excluded {
		for _, p := range policies {
			if p.Namespace == e.Namespace {
				t.Errorf("%s is %s's to write rules for, and %s is in it", e.Namespace, e.Owner, p.Name)
			}
		}
	}
	for _, want := range []string{"system-mail", "system-mail-dmz"} {
		found := false
		for _, e := range inv.Excluded {
			found = found || e.Namespace == want
		}
		if !found {
			t.Errorf("%s is no longer listed as excluded", want)
		}
	}
}

func nsLabels(name string) map[string]string {
	l := map[string]string{nameLabel: name}
	switch {
	case strings.HasPrefix(name, "kernel-"):
		l[layout.LabelTier] = string(layout.TierKernel)
		l[layout.LabelFunction] = strings.TrimPrefix(name, "kernel-")
	case strings.HasSuffix(name, "-dmz") && strings.HasPrefix(name, "tenant-"):
		l[layout.LabelTier] = string(layout.TierTenantDMZ)
	case strings.HasPrefix(name, "tenant-"):
		l[layout.LabelTier] = string(layout.TierTenant)
	case strings.HasSuffix(name, "-dmz") && strings.HasPrefix(name, "system-"):
		l[layout.LabelTier] = string(layout.TierSystemDMZ)
	case strings.HasPrefix(name, "system-"):
		l[layout.LabelTier] = string(layout.TierSystem)
	}
	return l
}

func at(namespace string, podLabels map[string]string) pod {
	return pod{namespace: namespace, nsLabels: nsLabels(namespace), labels: podLabels}
}

// sources are pods a peer stands for: one per namespace it names.
func sources(p Peer) []pod {
	var out []pod
	if p.Namespace != "" {
		out = append(out, at(p.Namespace, p.PodLabels))
	}
	for _, ns := range p.Namespaces {
		out = append(out, at(ns, p.PodLabels))
	}
	for _, tier := range p.Tier {
		if tier == "tenant" {
			out = append(out, at("tenant-acme", map[string]string{"app.kubernetes.io/instance": "tenant-acme-desktop"}))
			out = append(out, at("tenant-platform", map[string]string{"app.kubernetes.io/instance": "tenant-platform-admin-console"}))
		}
	}
	return out
}

// Every caller the inventory names is admitted by the policies made from it,
// and every port it calls open admits a pod from nowhere in particular.
func TestEveryInventoriedCallerIsAdmitted(t *testing.T) {
	inv := load(t)
	policies := inv.Policies()
	stranger := at("default", map[string]string{"app": "stranger"})
	for _, ns := range inv.Namespaces {
		for _, w := range ns.Workloads {
			dst := at(ns.Name, w.PodLabels)
			for _, p := range w.Ports {
				for _, f := range p.From {
					srcs := sources(inv.Peers[f.Peer])
					if len(srcs) == 0 {
						t.Errorf("%s/%s port %d: peer %s names nobody", ns.Name, w.Name, p.Port, f.Peer)
					}
					for _, src := range srcs {
						if !admitted(policies, src, dst, p.Port) {
							t.Errorf("%s/%s port %d refuses %s from %s %v", ns.Name, w.Name, p.Port, f.Peer, src.namespace, src.labels)
						}
					}
				}
				if p.Open != "" && !admitted(policies, stranger, dst, p.Port) {
					t.Errorf("%s/%s port %d is open and refuses a pod of another namespace", ns.Name, w.Name, p.Port)
				}
				if p.Closed != "" && admitted(policies, stranger, dst, p.Port) {
					t.Errorf("%s/%s port %d is closed and admits a pod of another namespace", ns.Name, w.Name, p.Port)
				}
				if len(p.From) > 0 && admitted(policies, stranger, dst, p.Port) {
					t.Errorf("%s/%s port %d names its callers and admits a pod of another namespace", ns.Name, w.Name, p.Port)
				}
			}
		}
	}
}

var (
	chart = func(component string) map[string]string {
		return map[string]string{"app.kubernetes.io/name": "gentian-os", "app.kubernetes.io/instance": "gentian-os", "app.kubernetes.io/component": component}
	}
	envoyPod    = map[string]string{"app.kubernetes.io/name": "envoy", "app.kubernetes.io/component": "proxy", "app.kubernetes.io/managed-by": "envoy-gateway"}
	openfgaPod  = map[string]string{"app.kubernetes.io/name": "openfga", "app.kubernetes.io/instance": "gentian-openfga"}
	openbaoPod  = map[string]string{"app.kubernetes.io/name": "openbao", "app.kubernetes.io/instance": "openbao"}
	transitPod  = map[string]string{"app.kubernetes.io/name": "openbao", "app.kubernetes.io/instance": "openbao-transit"}
	keycloakPod = map[string]string{"app.kubernetes.io/name": "keycloakx", "app.kubernetes.io/instance": "gentian-idp-keycloak"}
	postgresPod = map[string]string{"cnpg.io/cluster": "kernel-postgres", "cnpg.io/podRole": "instance"}
	tenantApp   = at("tenant-acme", map[string]string{"gentianos.io/app": "nextcloud"})
	tenantDMZ   = at("tenant-acme-dmz", map[string]string{"app.kubernetes.io/name": "perimeter-proxy"})
)

// The flows a fresh install and a running cluster cannot do without, each by
// the pods' real labels. The inventory's own test above asks the same of
// every line; these are written out so that a reader sees them, and so that
// the flows inside a namespace -- which the inventory lists as `also` -- are
// asked too.
func TestTheFlowsAClusterNeeds(t *testing.T) {
	policies := load(t).Policies()
	for _, f := range []struct {
		what string
		src  pod
		dst  pod
		port int32
	}{
		{"Envoy asks the bouncer", at("kernel-edge", envoyPod), at("kernel-edge", chart("bouncer")), 9001},
		{"Envoy fetches its configuration", at("kernel-edge", envoyPod), at("kernel-edge", map[string]string{"control-plane": "envoy-gateway"}), 18000},
		{"the bouncer asks OpenFGA", at("kernel-edge", chart("bouncer")), at("kernel-authorization", openfgaPod), 8080},
		{"the bouncer reads Keycloak's keys", at("kernel-edge", chart("bouncer")), at("kernel-authentication", keycloakPod), 8080},
		{"Envoy routes id.<kernel> to Keycloak", at("kernel-edge", envoyPod), at("kernel-authentication", keycloakPod), 8080},
		{"Envoy routes headlamp.<kernel>", at("kernel-edge", envoyPod), at("kernel-observability", map[string]string{"app.kubernetes.io/name": "headlamp"}), 4466},
		{"Headlamp reaches kube-oidc-proxy", at("kernel-observability", map[string]string{"app.kubernetes.io/name": "headlamp"}), at("kernel-observability", map[string]string{"app.kubernetes.io/name": "kube-oidc-proxy"}), 8443},
		{"the operator writes the authorization model", at("kernel-control", chart("operator")), at("kernel-authorization", openfgaPod), 8080},
		{"the director asks OpenFGA", at("kernel-control", chart("director")), at("kernel-authorization", openfgaPod), 8080},
		{"the usher asks OpenFGA", at("kernel-control", chart("usher")), at("kernel-authorization", openfgaPod), 8080},
		{"the custodian asks OpenFGA", at("kernel-control", chart("custodian")), at("kernel-authorization", openfgaPod), 8080},
		{"the registrar asks OpenFGA", at("kernel-control", chart("registrar")), at("kernel-authorization", openfgaPod), 8080},
		{"the operator calls Keycloak's administration", at("kernel-control", chart("operator")), at("kernel-authentication", keycloakPod), 8080},
		{"the registrar calls Keycloak's administration", at("kernel-control", chart("registrar")), at("kernel-authentication", keycloakPod), 8080},
		{"the director reads Keycloak's keys", at("kernel-control", chart("director")), at("kernel-authentication", keycloakPod), 8080},
		{"provider-keycloak configures realms", at("kernel-provisioning", map[string]string{"pkg.crossplane.io/provider": "provider-keycloak"}), at("kernel-authentication", keycloakPod), 8080},
		{"a realm Job, unlabelled, calls Keycloak", at("kernel-authentication", map[string]string{"job-name": "realm-acme"}), at("kernel-authentication", keycloakPod), 8080},
		{"the sign-in sidecar reads the SAML descriptor", at("tenant-acme", map[string]string{"gentianos.io/sign-in-sidecar": "wiki"}), at("kernel-authentication", keycloakPod), 8080},
		{"Dovecot asks whether a token is good", at("system-mail", map[string]string{"app.kubernetes.io/name": "dovecot"}), at("kernel-authentication", keycloakPod), 8080},
		{"the operator reads the vault", at("kernel-control", chart("operator")), at("kernel-secrets", openbaoPod), 8200},
		{"the custodian writes the vault", at("kernel-control", chart("custodian")), at("kernel-secrets", openbaoPod), 8200},
		{"provider-vault configures the vault", at("kernel-provisioning", map[string]string{"pkg.crossplane.io/provider": "provider-vault"}), at("kernel-secrets", openbaoPod), 8200},
		{"External Secrets reads the vault", at("kernel-secrets", map[string]string{"app.kubernetes.io/name": "external-secrets"}), at("kernel-secrets", openbaoPod), 8200},
		{"the vault unseals through the seal", at("kernel-secrets", openbaoPod), at("kernel-seal", transitPod), 8200},
		{"Crossplane calls a function", at("kernel-provisioning", map[string]string{"app": "crossplane"}), at("kernel-provisioning", map[string]string{"pkg.crossplane.io/function": "function-go-templating"}), 9443},
		{"a component asks the rights check", tenantApp, at("kernel-edge", chart("bouncer")), 8082},
		{"the desktop calls the director", at("tenant-platform", map[string]string{"app.kubernetes.io/instance": "tenant-platform-desktop"}), at("kernel-control", chart("director")), 8080},
		{"the desktop calls the usher", at("tenant-acme", map[string]string{"app.kubernetes.io/instance": "tenant-acme-desktop"}), at("kernel-control", chart("usher")), 8080},
		{"the administration console calls the custodian", at("tenant-platform", map[string]string{"app.kubernetes.io/instance": "tenant-platform-admin-console"}), at("kernel-control", chart("custodian")), 9444},
		{"the administration console calls the registrar", at("tenant-platform", map[string]string{"app.kubernetes.io/instance": "tenant-platform-admin-console"}), at("kernel-control", chart("registrar")), 9445},
		{"CloudNativePG's operator and an instance", at("kernel-data", map[string]string{"app.kubernetes.io/name": "cloudnative-pg"}), at("kernel-data", postgresPod), 8000},
		{"anybody reaches a public listener", tenantApp, at("kernel-edge", envoyPod), 10443},
		{"the internet reaches a public listener", at("default", nil), at("kernel-edge", envoyPod), 10080},
	} {
		if !admitted(policies, f.src, f.dst, f.port) {
			t.Errorf("refused: %s (%s -> %s:%d)", f.what, f.src.namespace, f.dst.namespace, f.port)
		}
	}
}

// What these rules exist to refuse.
func TestWhatIsRefused(t *testing.T) {
	policies := load(t).Policies()
	certManager := at("kernel-edge", map[string]string{"app.kubernetes.io/name": "cert-manager"})
	argocd := at("kernel-gitops", map[string]string{"app.kubernetes.io/name": "argocd-repo-server"})
	jobGC := at("kernel-control", map[string]string{"gentianos.io/component": "job-gc"})
	for _, f := range []struct {
		what string
		src  pod
		dst  pod
		port int32
	}{
		{"a tenant's app asks OpenFGA", tenantApp, at("kernel-authorization", openfgaPod), 8080},
		{"a tenant's app reaches the vault", tenantApp, at("kernel-secrets", openbaoPod), 8200},
		{"a tenant's app reaches the seal", tenantApp, at("kernel-seal", transitPod), 8200},
		{"a tenant's app asks the bouncer", tenantApp, at("kernel-edge", chart("bouncer")), 9001},
		{"a tenant's app reaches the Gateway's configuration", tenantApp, at("kernel-edge", map[string]string{"control-plane": "envoy-gateway"}), 18000},
		{"a tenant's app reaches the operator's listener", tenantApp, at("kernel-control", chart("operator")), 8082},
		{"a tenant's app reaches Keycloak's management port", tenantApp, at("kernel-authentication", keycloakPod), 9000},
		{"a tenant's app reaches Headlamp", tenantApp, at("kernel-observability", map[string]string{"app.kubernetes.io/name": "headlamp"}), 4466},
		{"a tenant's app reaches kube-oidc-proxy", tenantApp, at("kernel-observability", map[string]string{"app.kubernetes.io/name": "kube-oidc-proxy"}), 8443},
		{"a tenant's app reaches CloudNativePG's metrics", tenantApp, at("kernel-data", map[string]string{"app.kubernetes.io/name": "cloudnative-pg"}), 8080},
		{"a tenant's app reaches the image updater", tenantApp, at("kernel-gitops", map[string]string{"app.kubernetes.io/name": "argocd-image-updater"}), 8443},
		{"a publishing proxy reaches the director", tenantDMZ, at("kernel-control", chart("director")), 8080},
		{"a publishing proxy reaches the custodian", tenantDMZ, at("kernel-control", chart("custodian")), 9444},
		{"a publishing proxy reaches Keycloak", tenantDMZ, at("kernel-authentication", keycloakPod), 8080},
		{"a publishing proxy asks OpenFGA", tenantDMZ, at("kernel-authorization", openfgaPod), 8080},
		{"cert-manager asks the bouncer", certManager, at("kernel-edge", chart("bouncer")), 9001},
		{"cert-manager asks OpenFGA", certManager, at("kernel-authorization", openfgaPod), 8080},
		{"Envoy asks OpenFGA itself", at("kernel-edge", envoyPod), at("kernel-authorization", openfgaPod), 8080},
		{"Envoy reaches the vault", at("kernel-edge", envoyPod), at("kernel-secrets", openbaoPod), 8200},
		{"Argo CD reaches the vault", argocd, at("kernel-secrets", openbaoPod), 8200},
		{"Argo CD asks OpenFGA", argocd, at("kernel-authorization", openfgaPod), 8080},
		{"Keycloak reaches the vault", at("kernel-authentication", keycloakPod), at("kernel-secrets", openbaoPod), 8200},
		{"OpenFGA reaches the vault", at("kernel-authorization", openfgaPod), at("kernel-secrets", openbaoPod), 8200},
		{"the director reaches the vault", at("kernel-control", chart("director")), at("kernel-secrets", openbaoPod), 8200},
		{"another pod of the control namespace reaches the vault", jobGC, at("kernel-secrets", openbaoPod), 8200},
		{"another pod of the control namespace asks OpenFGA", jobGC, at("kernel-authorization", openfgaPod), 8080},
		{"another pod of the control namespace reaches the custodian", jobGC, at("kernel-control", chart("custodian")), 9444},
		{"a pod outside the layout reaches Keycloak", at("default", map[string]string{"app": "x"}), at("kernel-authentication", keycloakPod), 8080},
		{"a pod outside the layout reaches the director", at("default", map[string]string{"app": "x"}), at("kernel-control", chart("director")), 8080},
		{"a system namespace's pod reaches the director", at("system-s3", map[string]string{"app": "minio"}), at("kernel-control", chart("director")), 8080},
	} {
		if admitted(policies, f.src, f.dst, f.port) {
			t.Errorf("admitted: %s (%s -> %s:%d)", f.what, f.src.namespace, f.dst.namespace, f.port)
		}
	}
}

var evidencePath = regexp.MustCompile(`^([A-Za-z0-9_./-]+\.[a-z]+):[0-9]+$`)

// Evidence that names a file of this repository names one that exists.
func TestEvidenceNamesFilesThatExist(t *testing.T) {
	inv := load(t)
	var all []string
	for _, p := range inv.Peers {
		all = append(all, p.Evidence...)
	}
	for _, ns := range inv.Namespaces {
		for _, w := range ns.Workloads {
			all = append(all, w.Evidence...)
			for _, p := range w.Ports {
				all = append(all, p.Evidence...)
				for _, f := range p.From {
					all = append(all, f.Evidence...)
				}
			}
		}
	}
	for _, e := range all {
		m := evidencePath.FindStringSubmatch(e)
		if m == nil {
			continue // a sentence about an upstream source
		}
		if _, err := os.Stat(filepath.Join(repoRoot, m[1])); err != nil {
			t.Errorf("evidence %q: %v", e, err)
		}
	}
}

// admits reports whether the inventory lets anybody outside reach the port.
func (inv *Inventory) admits(namespace string, port int32) bool {
	for _, ns := range inv.Namespaces {
		if ns.Name != namespace {
			continue
		}
		for _, w := range ns.Workloads {
			for _, p := range w.Ports {
				if p.Port == port && p.Closed == "" {
					return true
				}
			}
		}
	}
	return false
}

// kernelAddress is one in-cluster address of a kernel service that a file of
// this repository builds or names.
type kernelAddress struct {
	namespace string
	port      int32
}

// The files that build or name an in-cluster address, and which kernel
// service each is a client of. A file that is not here and has such an
// address fails the test below: whoever adds a client of a kernel service
// says so here, and the inventory must admit it.
var inClusterAddressFiles = map[string][]kernelAddress{
	"charts/gentian-os/templates/bouncer.yaml":    {{"kernel-authentication", 8080}, {"kernel-authorization", 8080}},
	"charts/gentian-os/templates/custodian.yaml":  {{"kernel-authentication", 8080}, {"kernel-authorization", 8080}, {"kernel-secrets", 8200}},
	"charts/gentian-os/templates/deployment.yaml": {{"kernel-authorization", 8080}, {"kernel-secrets", 8200}, {"kernel-authentication", 8080}},
	"charts/gentian-os/templates/director.yaml":   {{"kernel-authentication", 8080}, {"kernel-authorization", 8080}, {"kernel-control", 8082}},
	"charts/gentian-os/templates/registrar.yaml":  {{"kernel-authentication", 8080}, {"kernel-authorization", 8080}},
	"charts/gentian-os/templates/usher.yaml":      {{"kernel-authentication", 8080}, {"kernel-authorization", 8080}, {"kernel-control", 8082}},
	// The registrar's database: kernel-postgres, by its own policy.
	"charts/gentian-os/templates/externalsecret-registrar-database.yaml": {{"kernel-data", 5432}},
	// Addresses handed on to compositions, not dialled from here.
	"charts/gentian-os/templates/kernel-services-configmap.yaml": {},
	// The webhook's certificate names, not a client.
	"charts/gentian-os/templates/webhook-cert-manager.yaml": {},
	// A comment naming the vault's address.
	"cmd/main.go": {{"kernel-secrets", 8200}},
	// The desktop's database: kernel-postgres or the tenants' server.
	"internal/controller/component_desktop.go": {{"kernel-data", 5432}},
	// The same database, for the units of a backup and the Job of a deletion
	// that run beside kernel-postgres as the database's owner.
	"internal/backup/desktop.go": {{"kernel-data", 5432}},
	// The tenants' MariaDB, in a system namespace.
	"internal/controller/tenantrestore_retained.go": {},
	// The proxy's upstream is a tenant's Service.
	"internal/controller/component_perimeter.go": {},
	// The addresses a component is handed: director, usher, custodian, registrar.
	"internal/controller/component_reconciler.go": {{"kernel-control", 8080}, {"kernel-control", 9444}, {"kernel-control", 9445}},
	// The tunnel's origin: the Gateway's own Service.
	"internal/controller/gateway_tunnel.go": {{"kernel-edge", 10443}},
	// A tenant's app to another app of the same tenant.
	"internal/controller/integration_binding_controller.go": {},
	// The mail servers', in the mail namespaces.
	"internal/controller/keycloak_tenant_smtp.go": {},
	"internal/controller/mail_reconciler.go":      {{"kernel-authentication", 8080}},
	// The address of the bouncer's rights check, handed to a component.
	"internal/controller/rights_check.go": {{"kernel-edge", 8082}},
	// The sidecar reads the realm's SAML descriptor.
	"internal/controller/signin_sidecar.go": {{"kernel-authentication", 8080}},
	// The tenants' stores, in the system namespaces.
	"internal/controller/tenant_data_plane_manifests.go": {},
	// Recognises an in-cluster address; dials none.
	"internal/custodian/http.go":             {},
	"internal/director/catalogue/address.go": {},
	// The model gateway, in a system namespace.
	"internal/modelgateway/modelgateway.go": {},
}

var inClusterAddress = regexp.MustCompile(`\.svc(\.cluster\.local)?\b`)

// Every Go file and every template of the operator's chart that carries an
// in-cluster address is listed above, and every kernel address listed is one
// the inventory admits somebody to.
func TestEveryClientOfAKernelServiceIsInventoried(t *testing.T) {
	inv := load(t)
	found := map[string]bool{}
	for _, dir := range []string{"internal", "cmd", "charts/gentian-os/templates"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(repoRoot, path)
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "definitions" || rel == "internal/kernel/kernelnet" {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".go", ".yaml", ".tpl":
			default:
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if inClusterAddress.Match(raw) {
				found[rel] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var missing []string
	for file := range found {
		if _, ok := inClusterAddressFiles[file]; !ok {
			missing = append(missing, file)
		}
	}
	sort.Strings(missing)
	for _, file := range missing {
		t.Errorf("%s carries an in-cluster address and is not classified: add it to inClusterAddressFiles with the kernel services it is a client of, and the caller to inventory.yaml", file)
	}
	for file, addresses := range inClusterAddressFiles {
		if !found[file] {
			t.Errorf("%s is classified and carries no in-cluster address any more: remove it", file)
		}
		for _, a := range addresses {
			if !inv.admits(a.namespace, a.port) {
				t.Errorf("%s is a client of %s port %d, which the inventory admits nobody to", file, a.namespace, a.port)
			}
		}
	}
}

// The kernel's rules and the tenants' baseline name the same Envoy pods.
func TestTheEnvoyPeerIsTheBaselines(t *testing.T) {
	got := load(t).Peers["envoy"]
	want := netpolicy.EdgeProxyPodLabels()
	if got.Namespace != layout.Kernel(layout.Edge) || len(got.PodLabels) != len(want) {
		t.Fatalf("peer envoy is %s %v, the baseline's is %s %v", got.Namespace, got.PodLabels, layout.Kernel(layout.Edge), want)
	}
	for k, v := range want {
		if got.PodLabels[k] != v {
			t.Fatalf("peer envoy is %v, the baseline's is %v", got.PodLabels, want)
		}
	}
}
