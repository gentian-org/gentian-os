/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package catalogue

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

const someDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// An artifact reference is checked as it is written, as an address is: the
// registry is a public host on 443, and the artifact is named exactly.
func TestAnArtifactReferenceIsCheckedAsItIsWritten(t *testing.T) {
	for raw, want := range map[string]OCIReference{
		"ghcr.io/acme/catalogues/notes:1.0.0":     {Host: "ghcr.io", Path: "acme/catalogues/notes", Tag: "1.0.0"},
		"ghcr.io:443/acme/notes:v1":               {Host: "ghcr.io", Path: "acme/notes", Tag: "v1"},
		"Registry.Example.com/acme/notes:latest":  {Host: "registry.example.com", Path: "acme/notes", Tag: "latest"},
		"ghcr.io/acme/notes@" + someDigest:        {Host: "ghcr.io", Path: "acme/notes", Digest: someDigest},
		"ghcr.io/acme/notes:1@" + someDigest:      {Host: "ghcr.io", Path: "acme/notes", Tag: "1", Digest: someDigest},
		"registry.example.com/a_b/c.d/e--f:x.y-z": {Host: "registry.example.com", Path: "a_b/c.d/e--f", Tag: "x.y-z"},
	} {
		got, err := ParseOCIReference(raw)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("%s: %+v, want %+v", raw, got, want)
		}
	}
	for raw, why := range map[string]string{
		"":                                                  "required",
		" ghcr.io/acme/notes:1":                             "whitespace",
		"oci://ghcr.io/acme/notes:1":                        "scheme",
		"https://ghcr.io/acme/notes:1":                      "scheme",
		"ghcr.io/acme/notes":                                "neither a tag nor a digest",
		"notes:1":                                           "no registry host",
		"ghcr.io:5000/acme/notes:1":                         "port 5000",
		"localhost/acme/notes:1":                            "not a host name",
		"registry/acme/notes:1":                             "not a host name",
		"127.0.0.1/acme/notes:1":                            "127.0.0.0/8",
		"10.0.0.1/acme/notes:1":                             "10.0.0.0/8",
		"169.254.169.254/latest/meta:1":                     "169.254.0.0/16",
		"registry.kernel-gitops.svc/a/b:1":                  ".svc",
		"zot.cluster.local/a/b:1":                           ".cluster.local",
		"metadata.google.internal/a/b:1":                    ".internal",
		"ghcr.io/Acme/notes:1":                              "lower-case names",
		"ghcr.io/acme/../notes:1":                           "lower-case names",
		"ghcr.io/acme//notes:1":                             "lower-case names",
		"ghcr.io/:1":                                        "no repository",
		"ghcr.io/acme/notes:-bad":                           "not a tag",
		"ghcr.io/acme/notes@sha256:abc":                     "digest",
		"ghcr.io/acme/notes@md5:" + strings.Repeat("a", 32): "digest",
		"user:pw@ghcr.io/acme/notes:1":                      "digest",
	} {
		_, err := ParseOCIReference(raw)
		if err == nil {
			t.Errorf("%q was accepted", raw)
			continue
		}
		if !errors.Is(err, ErrArtifactRefused) && !errors.Is(err, ErrAddressRefused) {
			t.Errorf("%q: %v is neither refusal", raw, err)
		}
		if !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v, want it to say %q", raw, err, why)
		}
	}
}

// A repository's credential is sent to that repository's registry, for an
// artifact inside its address, and to nothing else.
func TestAnArtifactIsReadOnlyWithTheRepositoryItIsIn(t *testing.T) {
	ref, err := ParseOCIReference("ghcr.io/acme/catalogues/notes:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, inside := range []string{"oci://ghcr.io/acme", "oci://ghcr.io/acme/", "ghcr.io/acme/catalogues", "oci://ghcr.io",
		"oci://GHCR.io:443/acme", "https://ghcr.io/acme/catalogues/notes"} {
		if err := CheckArtifactInRegistry(ref, inside); err != nil {
			t.Errorf("%s: %v", inside, err)
		}
	}
	for _, outside := range []string{"oci://ghcr.io/acmeco", "oci://ghcr.io/other", "oci://registry.example.com/acme",
		"oci://ghcr.io.evil.example/acme", "oci://ghcr.io/acme/catalogues/notes/more", "", "oci://", "oci://user@ghcr.io/acme"} {
		err := CheckArtifactInRegistry(ref, outside)
		if !errors.Is(err, ErrRegistryRefused) {
			t.Errorf("%q: %v, want a refusal", outside, err)
		}
	}
}

type entry struct {
	name     string
	typeflag byte
	body     string
	link     string
	size     int64
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		size := int64(len(e.body))
		if e.typeflag != tar.TypeReg {
			size = 0
		}
		if e.size != 0 {
			size = e.size
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Linkname: e.link, Mode: 0o644, Size: size}
		if e.typeflag == tar.TypeXGlobalHeader {
			hdr = &tar.Header{Typeflag: e.typeflag, PAXRecords: map[string]string{"comment": "a commit id"}}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg && e.size == 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// A catalogue's layer is the files of a catalogue and nothing else.
func TestALayerUnpacksToItsFiles(t *testing.T) {
	files, err := untarCatalogue(tarball(t,
		entry{name: "./", typeflag: tar.TypeDir},
		entry{name: "./index.yaml", typeflag: tar.TypeReg, body: "entries: []\n"},
		entry{name: "./profiles/", typeflag: tar.TypeDir},
		entry{name: "./profiles/acme-notes.yaml", typeflag: tar.TypeReg, body: "kind: ComponentProfile\n"},
		entry{name: "packages/acme-notes.yaml", typeflag: tar.TypeReg, body: "kind: AppPackage\n"},
		entry{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || string(files["index.yaml"]) != "entries: []\n" ||
		string(files["profiles/acme-notes.yaml"]) != "kind: ComponentProfile\n" || files["packages/acme-notes.yaml"] == nil {
		t.Fatalf("unpacked %v", files)
	}
}

// What a catalogue's layer may not hold refuses the whole of it.
func TestALayerThatIsNotACatalogueIsRefused(t *testing.T) {
	big := strings.Repeat("x", maxBundle+1)
	for why, entries := range map[string][]entry{
		"climbs out":            {{name: "../index.yaml", typeflag: tar.TypeReg, body: "x"}},
		"climbs out, deeper":    {{name: "profiles/../../etc/passwd", typeflag: tar.TypeReg, body: "x"}},
		"an absolute path":      {{name: "/index.yaml", typeflag: tar.TypeReg, body: "x"}},
		"a backslash":           {{name: "profiles\\x.yaml", typeflag: tar.TypeReg, body: "x"}},
		"an empty component":    {{name: "profiles//x.yaml", typeflag: tar.TypeReg, body: "x"}},
		"a symbolic link":       {{name: "index.yaml", typeflag: tar.TypeSymlink, link: "/etc/passwd"}},
		"a hard link":           {{name: "index.yaml", typeflag: tar.TypeLink, link: "profiles/x.yaml"}},
		"a device":              {{name: "dev", typeflag: tar.TypeChar}},
		"a pipe":                {{name: "fifo", typeflag: tar.TypeFifo}},
		"written twice":         {{name: "index.yaml", typeflag: tar.TypeReg, body: "a"}, {name: "./index.yaml", typeflag: tar.TypeReg, body: "b"}},
		"a file over a dir":     {{name: "profiles/", typeflag: tar.TypeDir}, {name: "profiles", typeflag: tar.TypeReg, body: "x"}},
		"a file over the limit": {{name: "profiles/big.yaml", typeflag: tar.TypeReg, body: big}},
	} {
		_, err := untarCatalogue(tarball(t, entries...))
		if !errors.Is(err, ErrArtifactRefused) {
			t.Errorf("%s: %v, want a refusal", why, err)
		}
	}
	if _, err := untarCatalogue([]byte("not gzip")); !errors.Is(err, ErrArtifactRefused) {
		t.Errorf("a layer that is not gzip: %v", err)
	}
}

// A layer that unpacks to more than a catalogue can be is not read to its
// end: the limit is on what is unpacked, not on what was sent.
func TestALayerThatUnpacksPastTheLimitIsRefused(t *testing.T) {
	var entries []entry
	chunk := strings.Repeat("0", maxBundle)
	for i := 0; i*maxBundle <= maxTree; i++ {
		entries = append(entries, entry{name: "profiles/p" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".yaml",
			typeflag: tar.TypeReg, body: chunk})
	}
	blob := tarball(t, entries...)
	if len(blob) > maxLayer {
		t.Fatalf("the test's layer is %d bytes, more than a registry would be read for", len(blob))
	}
	if _, err := untarCatalogue(blob); !errors.Is(err, ErrArtifactRefused) || !strings.Contains(err.Error(), "unpacks to more than") {
		t.Fatalf("%v, want a refusal for its unpacked size", err)
	}
	var many []entry
	for i := 0; i <= maxEntries; i++ {
		many = append(many, entry{name: "d" + strings.Repeat("a", i%50) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i/676)) + "/", typeflag: tar.TypeDir})
	}
	if _, err := untarCatalogue(tarball(t, many...)); !errors.Is(err, ErrArtifactRefused) || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("%v, want a refusal for its number of entries", err)
	}
}

func manifest(artifactType, configType, layers string) []byte {
	return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"` + artifactType +
		`","config":{"mediaType":"` + configType + `","digest":"` + someDigest + `","size":2},"layers":[` + layers + `]}`)
}

func layer(mediaType string, size int) string {
	return `{"mediaType":"` + mediaType + `","digest":"` + someDigest + `","size":` + strconv.Itoa(size) + `}`
}

// A manifest is a catalogue's when it says so and holds one layer of the
// catalogue's type.
func TestAManifestIsACataloguesOrRefused(t *testing.T) {
	ok := layer(OCILayerType, 100)
	if got, err := checkManifest(manifest(OCIArtifactType, "application/vnd.oci.empty.v1+json", ok), ""); err != nil || got.Size != 100 {
		t.Fatalf("an artifact manifest: %v %v", got, err)
	}
	// A registry that keeps no artifact type: the config's media type says it.
	if _, err := checkManifest(manifest("", OCIArtifactType, ok), ""); err != nil {
		t.Fatalf("a manifest typed by its config: %v", err)
	}
	for why, raw := range map[string][]byte{
		"another artifact type":   manifest("application/vnd.example.other", "application/vnd.oci.empty.v1+json", ok),
		"an image":                manifest("", "application/vnd.oci.image.config.v1+json", ok),
		"two layers":              manifest(OCIArtifactType, "x", ok+","+ok),
		"no layer":                manifest(OCIArtifactType, "x", ""),
		"a layer of another type": manifest(OCIArtifactType, "x", layer("application/vnd.oci.image.layer.v1.tar+gzip", 100)),
		"a layer too large":       manifest(OCIArtifactType, "x", layer(OCILayerType, maxLayer+1)),
		"a layer of no size":      manifest(OCIArtifactType, "x", layer(OCILayerType, 0)),
		"a layer of another digest": []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"` +
			OCIArtifactType + `","layers":[{"mediaType":"` + OCILayerType + `","digest":"sha512:abc","size":1}]}`),
		"an index": []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`),
		"schema 1": []byte(`{"schemaVersion":1}`),
		"no JSON":  []byte(`<html>`),
	} {
		if _, err := checkManifest(raw, ""); !errors.Is(err, ErrArtifactRefused) {
			t.Errorf("%s: %v, want a refusal", why, err)
		}
	}
}

// A blob's redirect is followed only to where a catalogue could be fetched
// from at all.
func TestABlobIsRedirectedOnlyToAPublicHTTPSHost(t *testing.T) {
	from, _ := url.Parse("https://ghcr.io/v2/acme/notes/blobs/" + someDigest)
	if next, err := redirectTarget(from, "https://pkg-containers.githubusercontent.com/ghcr1/blobs/sha256:x?se=1&sig=abc"); err != nil ||
		!strings.HasPrefix(next, "https://pkg-containers.githubusercontent.com/") {
		t.Fatalf("a public storage host: %s %v", next, err)
	}
	if next, err := redirectTarget(from, "/v2/acme/notes/blobs/other"); err != nil || next != "https://ghcr.io/v2/acme/notes/blobs/other" {
		t.Fatalf("a relative redirect: %s %v", next, err)
	}
	for _, location := range []string{
		"http://storage.example.com/blob", "https://10.0.0.1/blob", "https://storage.example.com:8443/blob",
		"https://user:pw@storage.example.com/blob", "https://minio.kernel-data.svc/blob", "https://169.254.169.254/latest",
		"https://localhost/blob", "",
	} {
		if _, err := redirectTarget(from, location); err == nil {
			t.Errorf("%q was followed", location)
		}
	}
}

// The token realm a registry names is asked only on the registry's own
// registrable domain, so the login goes nowhere else.
func TestARealmIsOfTheRegistrysOwnDomain(t *testing.T) {
	for _, c := range []struct {
		realm, registry string
		same            bool
	}{
		{"ghcr.io", "ghcr.io", true},
		{"auth.docker.io", "registry-1.docker.io", true},
		{"registry.example.com", "auth.example.com", true},
		{"ghcr.io.evil.example", "ghcr.io", false},
		{"evil.io", "ghcr.io", false},
		{"token.example.org", "registry.example.com", false},
		{"example.github.io", "other.github.io", false}, // github.io is a public suffix
	} {
		if got := sameRegistrableDomain(c.realm, c.registry); got != c.same {
			t.Errorf("%s for %s: %v, want %v", c.realm, c.registry, got, c.same)
		}
	}
}

func TestAChallengeIsRead(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:acme/notes:pull"`)
	if scheme != "bearer" || params["realm"] != "https://ghcr.io/token" || params["service"] != "ghcr.io" ||
		params["scope"] != "repository:acme/notes:pull" {
		t.Fatalf("%s %v", scheme, params)
	}
	if scheme, params := parseChallenge(`Basic realm=registry`); scheme != "basic" || params["realm"] != "registry" {
		t.Fatalf("%s %v", scheme, params)
	}
	if scheme, _ := parseChallenge(""); scheme != "" {
		t.Fatalf("an empty challenge is %q", scheme)
	}
}
