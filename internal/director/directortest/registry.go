/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package directortest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Registry is an OCI registry for tests: one TLS server that answers for
// every host name, as the hosts a test names it by, so a test can name a
// public-looking registry, a token realm and a blob store and reach them all.
//
// Client is how a fetcher reaches it. It dials the server whatever the host,
// and verifies the server's certificate as example.com's.
type Registry struct {
	srv *httptest.Server

	mu        sync.Mutex
	manifests map[string][]byte // "<host>/<path>:<reference>" -> manifest
	blobs     map[string][]byte // digest -> blob
	requests  []Request

	// Login is the user name and password the registry requires, empty for
	// one read anonymously. With Realm set it is asked for at the realm,
	// which hands out Token; without, it is asked for as basic auth.
	Login [2]string
	// Realm is the token realm the registry names in its challenge, e.g.
	// https://registry.example.com/token.
	Realm string
	// Token is the bearer token the realm hands out, or, without a realm and
	// a login, the token the registry accepts as it is.
	Token string
	// BlobsAt, when set, is the host a blob is redirected to:
	// https://<BlobsAt>/blobs/<digest>?signature=x.
	BlobsAt string
	// ManifestRedirect, when set, answers every manifest with a redirect
	// there.
	ManifestRedirect string
	// Tamper changes what the registry serves: "manifest" serves a manifest
	// other than the one stored under a digest, "layer" serves other bytes
	// for the layer, "digest-header" states another Docker-Content-Digest.
	Tamper string
}

// Request is one request the registry was sent.
type Request struct {
	Host, Path, Authorization string
}

// NewRegistry starts one.
func NewRegistry(t *testing.T) *Registry {
	t.Helper()
	r := &Registry{manifests: map[string][]byte{}, blobs: map[string][]byte{}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

// Client reaches the registry for any host name on port 443.
func (r *Registry) Client() *http.Client {
	base := r.srv.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig = base.TLSClientConfig.Clone()
	base.TLSClientConfig.ServerName = "example.com"
	addr := r.srv.Listener.Addr().String()
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	base.Proxy = nil
	return &http.Client{
		Transport: base,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Requests is what the registry was sent, in order.
func (r *Registry) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.requests...)
}

// Artifact types a catalogue is pushed with.
const (
	CatalogueArtifactType = "application/vnd.gentianos.catalogue.v1"
	CatalogueLayerType    = "application/vnd.gentianos.catalogue.v1.tar+gzip"
)

// CatalogueLayer packs files, path -> body, as a catalogue's layer: a gzip'd
// tar, in path order, each entry below "./".
func CatalogueLayer(t *testing.T, files map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Push stores layer as the one layer of a catalogue artifact at
// <host>/<path>:<tag>, and answers the manifest's digest.
func (r *Registry) Push(t *testing.T, host, path, tag string, layer []byte) string {
	t.Helper()
	return r.PushManifest(t, host, path, tag, CatalogueManifest(layer, CatalogueArtifactType, CatalogueLayerType), layer)
}

// CatalogueManifest is the manifest oras writes for one layer.
func CatalogueManifest(layer []byte, artifactType, layerType string) []byte {
	sum := sha256.Sum256(layer)
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"artifactType":  artifactType,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.empty.v1+json",
			"digest":    "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
			"size":      2,
		},
		"layers": []any{map[string]any{
			"mediaType": layerType,
			"digest":    "sha256:" + hex.EncodeToString(sum[:]),
			"size":      len(layer),
		}},
	}
	raw, _ := json.Marshal(m)
	return raw
}

// PushManifest stores a manifest as it is, and the blob it names.
func (r *Registry) PushManifest(t *testing.T, host, path, tag string, manifest, blob []byte) string {
	t.Helper()
	sum := sha256.Sum256(manifest)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	blobSum := sha256.Sum256(blob)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests[host+"/"+path+":"+tag] = manifest
	r.manifests[host+"/"+path+":"+digest] = manifest
	r.blobs["sha256:"+hex.EncodeToString(blobSum[:])] = blob
	return digest
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	host := strings.TrimSuffix(req.Host, ":443")
	r.mu.Lock()
	r.requests = append(r.requests, Request{Host: host, Path: req.URL.Path, Authorization: req.Header.Get("Authorization")})
	r.mu.Unlock()

	if r.Realm != "" && "https://"+host+req.URL.Path == strings.SplitN(r.Realm, "?", 2)[0] {
		if !r.loggedIn(req.Header.Get("Authorization")) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": r.Token})
		return
	}
	if strings.HasPrefix(req.URL.Path, "/blobs/") && r.BlobsAt != "" && host == r.BlobsAt {
		r.mu.Lock()
		blob, ok := r.blobs[strings.TrimPrefix(req.URL.Path, "/blobs/")]
		r.mu.Unlock()
		if !ok || req.URL.Query().Get("signature") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(blob)
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !r.authorized(req.Header.Get("Authorization")) {
		switch {
		case r.Realm != "":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service=%q,scope="repository:x:pull,push"`, r.Realm, host))
		case r.Login != [2]string{}:
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="none"`)
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if at := strings.Index(rest, "/manifests/"); at >= 0 {
		if r.ManifestRedirect != "" {
			w.Header().Set("Location", r.ManifestRedirect)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		key := host + "/" + rest[:at] + ":" + rest[at+len("/manifests/"):]
		r.mu.Lock()
		manifest, ok := r.manifests[key]
		r.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(manifest)
		stated := "sha256:" + hex.EncodeToString(sum[:])
		switch r.Tamper {
		case "manifest":
			manifest = append(append([]byte{}, manifest...), ' ')
		case "digest-header":
			stated = "sha256:" + strings.Repeat("0", 64)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", stated)
		_, _ = w.Write(manifest)
		return
	}
	if at := strings.Index(rest, "/blobs/"); at >= 0 {
		digest := rest[at+len("/blobs/"):]
		if r.BlobsAt != "" {
			w.Header().Set("Location", "https://"+r.BlobsAt+"/blobs/"+digest+"?signature=x")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		r.mu.Lock()
		blob, ok := r.blobs[digest]
		r.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Tamper == "layer" {
			// Other bytes of the same length.
			blob = append([]byte{}, blob...)
			blob[len(blob)/2] ^= 0xff
		}
		_, _ = w.Write(blob)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (r *Registry) basic() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(r.Login[0]+":"+r.Login[1]))
}

func (r *Registry) loggedIn(authorization string) bool {
	return r.Login == [2]string{} || authorization == r.basic()
}

func (r *Registry) authorized(authorization string) bool {
	switch {
	case r.Realm != "":
		return authorization == "Bearer "+r.Token
	case r.Login != [2]string{}:
		return authorization == r.basic()
	case r.Token != "":
		return authorization == "Bearer "+r.Token
	}
	return true
}
