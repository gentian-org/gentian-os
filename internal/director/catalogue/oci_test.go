/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package catalogue_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// A catalogue kept as an OCI artifact, pulled from a registry that plays
// every host a test names (directortest.Registry).

const registryHost = "registry.example.com"

// fixedRegistries answers one repository, as a declaration and a vault would.
type fixedRegistries map[string]catalogue.Registry

func (f fixedRegistries) Registry(_ context.Context, owner, repository string) (catalogue.Registry, error) {
	reg, ok := f[owner+"/"+repository]
	if !ok {
		return catalogue.Registry{}, catalogue.ErrRegistryRefused
	}
	return reg, nil
}

func ociCatalogue(t *testing.T, reg *dt.Registry, registries catalogue.Registries, artifact string) (*catalogue.Fetcher, catalogue.Source) {
	t.Helper()
	f := catalogue.NewFetcher()
	f.Client = reg.Client()
	f.Registries = registries
	return f, catalogue.Source{
		Key: "tenant/demo/acme", Name: "acme",
		OCI: &catalogue.OCISource{Owner: "demo", Repository: "acme", Artifact: artifact},
	}
}

func catalogueFiles() map[string]string {
	return map[string]string{
		"index.yaml": "entries:\n  - name: nextcloud-base-ce\n    version: \"1.0.0\"\n    edition: ie\n    digest: " +
			digestOf(nextcloudProfile) + "\n",
		"profiles/nextcloud-base-ce.yaml": nextcloudProfile,
	}
}

// What an address serves, an artifact serves: the index lists, and the
// bundle arrives at its digest.
func TestACatalogueArtifactIsListedAndInstalledFrom(t *testing.T) {
	reg := dt.NewRegistry(t)
	reg.Push(t, registryHost, "acme/catalogue", "1", dt.CatalogueLayer(t, catalogueFiles()))
	f, src := ociCatalogue(t, reg, fixedRegistries{"demo/acme": {Prefix: registryHost + "/acme"}}, registryHost+"/acme/catalogue:1")

	listing, err := f.Index(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Digest != digestOf(nextcloudProfile) {
		t.Fatalf("listing %+v", listing)
	}
	got, err := f.Fetch(context.Background(), src, "nextcloud-base-ce", digestOf(nextcloudProfile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != nextcloudProfile {
		t.Fatal("the bundle was not returned byte for byte")
	}
	// One pull for both: the artifact is kept as long as an index is.
	manifests := 0
	for _, req := range reg.Requests() {
		if strings.Contains(req.Path, "/manifests/") {
			manifests++
		}
	}
	if manifests != 1 {
		t.Fatalf("the manifest was asked for %d times", manifests)
	}
	// The bundle is still held to the digest the install names.
	if _, err := f.Fetch(context.Background(), src, "nextcloud-base-ce", digestOf("something else")); !errors.Is(err, catalogue.ErrDigestMismatch) {
		t.Fatalf("another digest: %v", err)
	}
	if _, err := f.Fetch(context.Background(), src, "absent", digestOf(nextcloudProfile)); !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("an entry the artifact does not hold: %v", err)
	}
}

// The login goes to the registry's token realm and the token to the
// registry, both of the repository's own registry, and nowhere else: a blob
// the registry hands to its storage is fetched there with no credential.
func TestTheCredentialGoesToTheRegistryAndNowhereElse(t *testing.T) {
	reg := dt.NewRegistry(t)
	reg.Login = [2]string{"acme-bot", "s3cret"}
	reg.Realm = "https://auth.example.com/token"
	reg.Token = "registry-token"
	reg.BlobsAt = "storage.example.net"
	reg.Push(t, registryHost, "acme/catalogue", "1", dt.CatalogueLayer(t, catalogueFiles()))
	f, src := ociCatalogue(t, reg, fixedRegistries{"demo/acme": {Prefix: registryHost + "/acme", Username: "acme-bot", Password: "s3cret"}},
		registryHost+"/acme/catalogue:1")
	if _, err := f.Fetch(context.Background(), src, "nextcloud-base-ce", digestOf(nextcloudProfile)); err != nil {
		t.Fatal(err)
	}
	sawStorage := false
	for _, req := range reg.Requests() {
		switch req.Host {
		case registryHost:
			if req.Authorization != "" && req.Authorization != "Bearer registry-token" {
				t.Errorf("the registry was sent %q", req.Authorization)
			}
		case "auth.example.com":
			if !strings.HasPrefix(req.Authorization, "Basic ") {
				t.Errorf("the realm was sent %q", req.Authorization)
			}
		case "storage.example.net":
			sawStorage = true
			if req.Authorization != "" {
				t.Errorf("the blob store was sent a credential: %q", req.Authorization)
			}
		default:
			t.Errorf("a request went to %s", req.Host)
		}
	}
	if !sawStorage {
		t.Fatal("the blob was not fetched from where the registry redirected it")
	}
}

// A registry that names a realm of another domain does not get the login
// sent there.
func TestARealmOfAnotherDomainIsNotSentTheLogin(t *testing.T) {
	reg := dt.NewRegistry(t)
	reg.Login = [2]string{"acme-bot", "s3cret"}
	reg.Realm = "https://token.example.org/token"
	reg.Token = "registry-token"
	reg.Push(t, registryHost, "acme/catalogue", "1", dt.CatalogueLayer(t, catalogueFiles()))
	f, src := ociCatalogue(t, reg, fixedRegistries{"demo/acme": {Prefix: registryHost + "/acme", Username: "acme-bot", Password: "s3cret"}},
		registryHost+"/acme/catalogue:1")
	_, err := f.Index(context.Background(), src)
	if !errors.Is(err, catalogue.ErrRegistryRefused) {
		t.Fatalf("%v, want a refusal", err)
	}
	for _, req := range reg.Requests() {
		if req.Host != registryHost || req.Authorization != "" {
			t.Errorf("a request went to %s with %q", req.Host, req.Authorization)
		}
	}
}

// A registry that asks for basic auth gets the login; a token-only
// credential is presented as it is and never traded.
func TestBasicAndTokenCredentials(t *testing.T) {
	basic := dt.NewRegistry(t)
	basic.Login = [2]string{"u", "p"}
	basic.Push(t, registryHost, "acme/catalogue", "1", dt.CatalogueLayer(t, catalogueFiles()))
	f, src := ociCatalogue(t, basic, fixedRegistries{"demo/acme": {Prefix: registryHost, Username: "u", Password: "p"}}, registryHost+"/acme/catalogue:1")
	if _, err := f.Index(context.Background(), src); err != nil {
		t.Fatalf("basic: %v", err)
	}

	token := dt.NewRegistry(t)
	token.Token = "pat"
	token.Push(t, registryHost, "acme/catalogue", "1", dt.CatalogueLayer(t, catalogueFiles()))
	f, src = ociCatalogue(t, token, fixedRegistries{"demo/acme": {Prefix: registryHost, Token: "pat"}}, registryHost+"/acme/catalogue:1")
	if _, err := f.Index(context.Background(), src); err != nil {
		t.Fatalf("token: %v", err)
	}
	f, src = ociCatalogue(t, token, fixedRegistries{"demo/acme": {Prefix: registryHost, Token: "wrong"}}, registryHost+"/acme/catalogue:1")
	if _, err := f.Index(context.Background(), src); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("a refused token: %v", err)
	}
}

// What the registry serves is held to its digests: the manifest to the one
// named or stated, the layer to the manifest's.
func TestARegistryServingSomethingElseIsRefused(t *testing.T) {
	for tamper, artifact := range map[string]string{"manifest": "@", "digest-header": ":1", "layer": ":1"} {
		reg := dt.NewRegistry(t)
		digest := reg.Push(t, registryHost, "acme/catalogue", "1", dt.CatalogueLayer(t, catalogueFiles()))
		reg.Tamper = tamper
		ref := registryHost + "/acme/catalogue" + artifact
		if artifact == "@" {
			ref += digest
		}
		f, src := ociCatalogue(t, reg, fixedRegistries{"demo/acme": {Prefix: registryHost}}, ref)
		if _, err := f.Index(context.Background(), src); !errors.Is(err, catalogue.ErrDigestMismatch) {
			t.Errorf("%s: %v, want a digest mismatch", tamper, err)
		}
	}
	// A tag is resolved to the digest of what is served, so one pinned by
	// digest is that manifest and no other.
	reg := dt.NewRegistry(t)
	digest := reg.Push(t, registryHost, "acme/catalogue", "1", dt.CatalogueLayer(t, catalogueFiles()))
	f, src := ociCatalogue(t, reg, fixedRegistries{"demo/acme": {Prefix: registryHost}}, registryHost+"/acme/catalogue:1@"+digest)
	if _, err := f.Index(context.Background(), src); err != nil {
		t.Fatalf("pinned by digest: %v", err)
	}
}

// A manifest is not redirected; an artifact that is not a catalogue is not
// read; an artifact outside the repository is not pulled at all.
func TestWhatIsNotACatalogueArtifactIsRefused(t *testing.T) {
	redirected := dt.NewRegistry(t)
	redirected.ManifestRedirect = "https://elsewhere.example.com/v2/x/manifests/1"
	f, src := ociCatalogue(t, redirected, fixedRegistries{"demo/acme": {Prefix: registryHost}}, registryHost+"/acme/catalogue:1")
	if _, err := f.Index(context.Background(), src); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("a redirected manifest: %v", err)
	}
	for _, r := range redirected.Requests() {
		if r.Host != registryHost {
			t.Fatalf("the redirect was followed to %s", r.Host)
		}
	}

	other := dt.NewRegistry(t)
	layer := dt.CatalogueLayer(t, catalogueFiles())
	other.PushManifest(t, registryHost, "acme/image", "1",
		dt.CatalogueManifest(layer, "application/vnd.example.other", dt.CatalogueLayerType), layer)
	f, src = ociCatalogue(t, other, fixedRegistries{"demo/acme": {Prefix: registryHost}}, registryHost+"/acme/image:1")
	if _, err := f.Index(context.Background(), src); !errors.Is(err, catalogue.ErrArtifactRefused) {
		t.Fatalf("another artifact type: %v", err)
	}

	outside := dt.NewRegistry(t)
	f, src = ociCatalogue(t, outside, fixedRegistries{"demo/acme": {Prefix: registryHost + "/acme", Username: "u", Password: "p"}},
		"elsewhere.example.com/acme/catalogue:1")
	if _, err := f.Index(context.Background(), src); !errors.Is(err, catalogue.ErrRegistryRefused) {
		t.Fatalf("an artifact outside the repository: %v", err)
	}
	if n := len(outside.Requests()); n != 0 {
		t.Fatalf("%d requests were made for an artifact outside the repository", n)
	}
}

// The repository is asked of the owner the source names, and of nobody
// else: a tenant's catalogue cannot be read with another tenant's
// repository, or the cluster's.
func TestACatalogueIsReadWithItsOwnersRepositoryOnly(t *testing.T) {
	reg := dt.NewRegistry(t)
	reg.Login = [2]string{"u", "p"}
	reg.Push(t, registryHost, "acme/catalogue", "1", dt.CatalogueLayer(t, catalogueFiles()))
	registries := fixedRegistries{
		"other/acme": {Prefix: registryHost, Username: "u", Password: "p"},
		"/acme":      {Prefix: registryHost, Username: "u", Password: "p"},
	}
	f, src := ociCatalogue(t, reg, registries, registryHost+"/acme/catalogue:1")
	if _, err := f.Index(context.Background(), src); !errors.Is(err, catalogue.ErrRegistryRefused) {
		t.Fatalf("demo's catalog was read with a repository that is not demo's: %v", err)
	}
	if n := len(reg.Requests()); n != 0 {
		t.Fatalf("%d requests were made", n)
	}
}

// VaultRegistries reads the credential at the owner's own path, and refuses
// a declaration that names another before the vault is asked.
func TestTheCredentialIsReadAtTheOwnersPathOnly(t *testing.T) {
	read := []string{}
	vault := map[string]map[string]string{
		"gentian-os/tenants/demo/repositories/acme": {"username": "u", "password": "p"},
		"gentian-os/tenants/demo/repositories/pat":  {"password": "token"},
		"gentian-os/kernel/repositories/ours":       {"username": "c", "password": "q"},
	}
	declared := map[string]catalogue.DeclaredRegistry{
		"demo/acme":   {URL: "oci://registry.example.com/acme", VaultPath: "gentian-os/tenants/demo/repositories/acme"},
		"demo/pat":    {URL: "oci://registry.example.com/acme", VaultPath: "gentian-os/tenants/demo/repositories/pat", Bearer: true},
		"demo/stolen": {URL: "oci://registry.example.com/acme", VaultPath: "gentian-os/tenants/other/repositories/acme"},
		"demo/kernel": {URL: "oci://registry.example.com/acme", VaultPath: "gentian-os/kernel/repositories/ours"},
		"demo/public": {URL: "oci://registry.example.com/public"},
		"demo/unset":  {URL: "oci://registry.example.com/acme", VaultPath: "gentian-os/tenants/demo/repositories/unset"},
		"/ours":       {URL: "oci://registry.example.com/ours", VaultPath: "gentian-os/kernel/repositories/ours"},
		"/demo-like":  {URL: "oci://registry.example.com/ours", VaultPath: "gentian-os/tenants/demo/repositories/acme"},
	}
	v := catalogue.VaultRegistries{
		Declared: func(_ context.Context, owner, name string) (catalogue.DeclaredRegistry, error) {
			d, ok := declared[owner+"/"+name]
			if !ok {
				return catalogue.DeclaredRegistry{}, errors.New("no such repository")
			}
			return d, nil
		},
		Read: func(_ context.Context, path string) (map[string]string, error) {
			read = append(read, path)
			if data, ok := vault[path]; ok {
				return data, nil
			}
			return nil, errors.New("not found")
		},
	}
	ctx := context.Background()
	if got, err := v.Registry(ctx, "demo", "acme"); err != nil || got.Username != "u" || got.Password != "p" || got.Prefix != "registry.example.com/acme" {
		t.Fatalf("demo's own: %+v %v", got, err)
	}
	if got, err := v.Registry(ctx, "demo", "pat"); err != nil || got.Token != "token" || got.Username != "" {
		t.Fatalf("a token: %+v %v", got, err)
	}
	if got, err := v.Registry(ctx, "demo", "public"); err != nil || got != (catalogue.Registry{Prefix: "registry.example.com/public"}) {
		t.Fatalf("a public registry: %+v %v", got, err)
	}
	if got, err := v.Registry(ctx, "", "ours"); err != nil || got.Username != "c" {
		t.Fatalf("the cluster's own: %+v %v", got, err)
	}
	read = nil
	for _, c := range [][2]string{{"demo", "stolen"}, {"demo", "kernel"}, {"", "demo-like"}} {
		if _, err := v.Registry(ctx, c[0], c[1]); !errors.Is(err, catalogue.ErrRegistryRefused) {
			t.Errorf("%s/%s: %v, want a refusal", c[0], c[1], err)
		}
	}
	if len(read) != 0 {
		t.Fatalf("the vault was asked for %v", read)
	}
	if _, err := v.Registry(ctx, "demo", "unset"); err == nil || !strings.Contains(err.Error(), "repository-unset") {
		t.Fatalf("a credential not set: %v", err)
	}
	if _, err := (catalogue.VaultRegistries{Declared: v.Declared}).Registry(ctx, "demo", "acme"); !errors.Is(err, catalogue.ErrRegistryRefused) {
		t.Fatalf("a director without a vault: %v", err)
	}
}
