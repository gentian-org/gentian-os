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
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// A catalogue that is an OCI artifact.
//
// The same catalogue an address serves -- index.yaml, profiles/<name>.yaml,
// optionally packages/<name>.yaml -- packed into one gzip'd tar and pushed to
// a registry as an artifact of its own type. What it adds is the one thing an
// address cannot have: it is read with a credential. Not one the declaration
// carries, and not one the director holds of its own, but the credential of a
// repository its owner declared -- the tenant's, for a tenant's catalogue; the
// cluster's, for one of the whole cluster -- read from the vault when it is
// pulled (Registries).
//
// Everything else stays as it is for an address. The registry's host is held
// to the same rules: https on 443, a public name that resolves to public
// addresses, checked again in the dialer; no proxy; no redirect, but for the
// two a registry cannot work without, each said where it is followed. The
// bytes are not trusted for having been read with a credential: the manifest
// and the layer are held to their digests, the tar to what a catalogue is
// (untarCatalogue), and each bundle in it to the digest an install names and
// to what its origin may bring, exactly as a bundle from an address is.
//
// The credential goes to the registry the repository was declared for and to
// nothing else: an artifact outside that repository's address is refused
// before a request is made, a token is asked for only at a realm of the
// registry's own registrable domain, and a redirect is followed without it.

// The artifact's types. A manifest whose artifact type, or for a registry
// that keeps no artifact type its config's media type, is not the first, or
// whose one layer is not the second, is not a catalogue.
const (
	OCIArtifactType = "application/vnd.gentianos.catalogue.v1"
	OCILayerType    = "application/vnd.gentianos.catalogue.v1.tar+gzip"
)

const (
	ociManifestType = "application/vnd.oci.image.manifest.v1+json"
	// maxManifest bounds a manifest. One with a config and a layer is well
	// under a kilobyte; annotations do not need more than this.
	maxManifest = 256 << 10
	// maxLayer bounds the packed catalogue, as the registry serves it.
	maxLayer = 10 << 20
	// maxTree bounds what the layer unpacks to. A catalogue of a thousand
	// apps at a few kilobytes each is a fraction of it; a gzip that unpacks
	// past it is refused rather than read to the end.
	maxTree = 32 << 20
	// maxEntries bounds how many entries the tar may have.
	maxEntries = 4096
	// maxTokenAnswer bounds a token realm's answer.
	maxTokenAnswer = 64 << 10
	// maxRedirects is how many hops a blob may take to the host that serves
	// it.
	maxRedirects = 3
)

// ErrArtifactRefused is an artifact reference the director will not pull
// from, or an artifact that is not a catalogue.
var ErrArtifactRefused = errors.New("catalogue: the artifact is refused")

// ErrRegistryRefused is a repository a catalogue cannot be read with: not
// declared for whoever the catalogue belongs to, not a registry, or not the
// registry the artifact is in.
var ErrRegistryRefused = errors.New("catalogue: the repository is not one this catalog can be read with")

// OCISource is where a catalogue kept as an OCI artifact is pulled from, and
// with whose credential.
type OCISource struct {
	// Owner is whose repository the credential is of: a tenant's name for a
	// tenant's catalogue, "" for a catalogue of the whole cluster. It is
	// taken from where the catalogue is declared, never from the declaration.
	Owner string
	// Repository is the name of the repository the owner declared.
	Repository string
	// Artifact is the reference, without a scheme:
	// <host>/<path>:<tag>, <host>/<path>@sha256:<hex>, or both.
	Artifact string
}

// Registry is a declared repository as a pull is made with it.
type Registry struct {
	// Prefix is what the repository covers, <host>[/<path>], without a
	// scheme. An artifact is pulled with this credential only when it lies
	// inside it.
	Prefix string
	// Username and Password are a registry login. Token, in their place, is
	// a token presented to the registry as it is. All empty is a registry
	// read anonymously.
	Username, Password, Token string
}

// Registries answers the repository a catalogue is read with: the one named
// repository declared by owner ("" for the cluster), with its credential.
// Nothing else is asked of it, so what it can answer is the scope of what a
// catalogue can be read with.
type Registries interface {
	Registry(ctx context.Context, owner, repository string) (Registry, error)
}

// OCIReference is an artifact reference, parsed.
type OCIReference struct {
	// Host is the registry's host name, lower case, without a port.
	Host string
	// Path is the repository inside the registry: org/catalogues/name.
	Path string
	// Tag and Digest name the artifact; at least one is set. A digest is
	// what is pulled when both are.
	Tag    string
	Digest string
}

// Reference is what the registry is asked for: the digest, else the tag.
func (r OCIReference) Reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

func (r OCIReference) String() string {
	out := r.Host + "/" + r.Path
	if r.Tag != "" {
		out += ":" + r.Tag
	}
	if r.Digest != "" {
		out += "@" + r.Digest
	}
	return out
}

var (
	ociPathComponent = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)
	ociTag           = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	ociHost          = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// ParseOCIReference reads an artifact reference as it is written and checks
// it, resolving nothing: a registry host as public as a catalogue's address
// must be (CheckAddress), a repository path of plain components, and a tag
// or a digest.
func ParseOCIReference(raw string) (OCIReference, error) {
	refuse := func(why string) (OCIReference, error) {
		return OCIReference{}, fmt.Errorf("%w: %s", ErrArtifactRefused, why)
	}
	if raw == "" || strings.TrimSpace(raw) != raw || len(raw) > 1024 {
		return refuse("an artifact is required, without leading or trailing whitespace")
	}
	if strings.Contains(raw, "://") {
		return refuse("it names a scheme; an artifact is <registry host>/<repository>:<tag> or @sha256:<digest>, and is always pulled over https")
	}
	var ref OCIReference
	rest := raw
	if at := strings.Index(rest, "@"); at >= 0 {
		digest, err := CanonicalDigest(rest[at+1:])
		if err != nil || !strings.HasPrefix(rest[at+1:], "sha256:") {
			return refuse("its digest is not sha256:<64 hex characters>")
		}
		ref.Digest, rest = digest, rest[:at]
	}
	slash := strings.Index(rest, "/")
	if slash <= 0 {
		return refuse("it names no registry host; write <registry host>/<repository>:<tag>")
	}
	host, repo := rest[:slash], rest[slash+1:]
	if colon := strings.LastIndex(repo, ":"); colon >= 0 {
		ref.Tag, repo = repo[colon+1:], repo[:colon]
		if !ociTag.MatchString(ref.Tag) {
			return refuse(fmt.Sprintf("its tag %q is not a tag", ref.Tag))
		}
	}
	if ref.Tag == "" && ref.Digest == "" {
		return refuse("it names neither a tag nor a digest; say which artifact (:<tag> or @sha256:<digest>)")
	}
	if h, port, found := strings.Cut(host, ":"); found {
		if port != "443" {
			return refuse("its registry is named with port " + port + "; a catalog is pulled on the https port, 443")
		}
		host = h
	}
	host = strings.ToLower(host)
	if !ociHost.MatchString(host) {
		// An address literal is refused here as well: a registry is named,
		// so that what it resolves to is checked as a name's is.
		return refuse("its registry host " + host + " is not a host name")
	}
	if err := CheckAddress("https://" + host + "/"); err != nil {
		return OCIReference{}, err
	}
	if repo == "" || len(repo) > 255 {
		return refuse("it names no repository inside the registry")
	}
	for _, part := range strings.Split(repo, "/") {
		if !ociPathComponent.MatchString(part) {
			return refuse(fmt.Sprintf("its repository %q is not lower-case names separated by '/'", repo))
		}
	}
	ref.Host, ref.Path = host, repo
	return ref, nil
}

// registryPrefix reads a repository's declared address as what it covers:
// oci://ghcr.io/acme is ghcr.io/acme.
func registryPrefix(declared string) (host, prefix string, err error) {
	p := strings.TrimSuffix(declared, "/")
	for _, scheme := range []string{"oci://", "https://"} {
		p = strings.TrimPrefix(p, scheme)
	}
	if p == "" || strings.Contains(p, "://") || strings.ContainsAny(p, "@?# ") {
		return "", "", fmt.Errorf("%w: its address %q is not a registry's", ErrRegistryRefused, declared)
	}
	host, rest, _ := strings.Cut(p, "/")
	host = strings.ToLower(strings.TrimSuffix(host, ":443"))
	if rest != "" {
		return host, host + "/" + rest, nil
	}
	return host, host, nil
}

// CheckArtifactInRegistry says whether an artifact lies inside the address a
// repository was declared with, or why not. The credential of a repository
// is sent to that repository and to nothing else, so an artifact anywhere
// else is not pulled with it, whoever declared both.
func CheckArtifactInRegistry(ref OCIReference, declared string) error {
	host, prefix, err := registryPrefix(declared)
	if err != nil {
		return err
	}
	if host != ref.Host {
		return fmt.Errorf("%w: the artifact is on %s and the repository is %s; a repository's credential is sent to its own registry only",
			ErrRegistryRefused, ref.Host, prefix)
	}
	full := ref.Host + "/" + ref.Path
	if full != prefix && !strings.HasPrefix(full, prefix+"/") {
		return fmt.Errorf("%w: the artifact %s is not inside the repository %s", ErrRegistryRefused, full, prefix)
	}
	return nil
}

// ── Pulling ─────────────────────────────────────────────────────────────────

// ociTree is a pulled catalogue: its files by path, and the manifest digest
// the tag resolved to.
type ociTree struct {
	at     time.Time
	digest string
	files  map[string][]byte
}

// ociCache holds pulled catalogs for as long as an index is kept: a listing
// and the install that follows it read one pull.
type ociCache struct {
	ociMu sync.Mutex
	trees map[string]ociTree
}

func ociKey(src Source) string {
	return src.Key + "\n" + src.OCI.Owner + "\n" + src.OCI.Repository + "\n" + src.OCI.Artifact
}

// ociFile is one file of a catalogue kept as an artifact.
func (f *Fetcher) ociFile(ctx context.Context, src Source, file string) ([]byte, error) {
	tree, err := f.ociTree(ctx, src)
	if err != nil {
		return nil, err
	}
	body, ok := tree.files[file]
	if !ok {
		return nil, fmt.Errorf("catalogue: %s: %w", src.OCI.Artifact, fs.ErrNotExist)
	}
	return body, nil
}

func (f *Fetcher) ociTree(ctx context.Context, src Source) (ociTree, error) {
	key := ociKey(src)
	f.ociMu.Lock()
	cached, ok := f.trees[key]
	f.ociMu.Unlock()
	if ok && time.Since(cached.at) <= indexTTL {
		return cached, nil
	}
	tree, err := f.pull(ctx, src.OCI)
	if err != nil {
		return ociTree{}, err
	}
	f.ociMu.Lock()
	if f.trees == nil {
		f.trees = map[string]ociTree{}
	}
	f.trees[key] = tree
	f.ociMu.Unlock()
	return tree, nil
}

// pull reads one catalogue artifact: the reference is checked, the
// repository is asked for, the artifact is checked to lie inside it, and only
// then is a request made.
func (f *Fetcher) pull(ctx context.Context, src *OCISource) (ociTree, error) {
	ref, err := ParseOCIReference(src.Artifact)
	if err != nil {
		return ociTree{}, err
	}
	if f.Registries == nil {
		return ociTree{}, fmt.Errorf("%w: this director reads no repository for a catalog", ErrRegistryRefused)
	}
	reg, err := f.Registries.Registry(ctx, src.Owner, src.Repository)
	if err != nil {
		return ociTree{}, err
	}
	if err := CheckArtifactInRegistry(ref, reg.Prefix); err != nil {
		return ociTree{}, err
	}
	s := &registrySession{client: f.Client, ref: ref, reg: reg}
	if reg.Token != "" {
		s.authorization = "Bearer " + reg.Token
	}

	manifestURL := "https://" + ref.Host + "/v2/" + ref.Path + "/manifests/" + ref.Reference()
	raw, header, err := s.get(ctx, manifestURL, ociManifestType, maxManifest, false)
	if err != nil {
		return ociTree{}, err
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if ref.Digest != "" && digest != ref.Digest {
		return ociTree{}, fmt.Errorf("%w: %s: the manifest served does not hash to the digest named", ErrDigestMismatch, ref)
	}
	if stated := header.Get("Docker-Content-Digest"); stated != "" {
		if d, err := CanonicalDigest(stated); err != nil || d != digest {
			return ociTree{}, fmt.Errorf("%w: %s: the registry states a digest the manifest does not hash to", ErrDigestMismatch, ref)
		}
	}
	layer, err := checkManifest(raw, header.Get("Content-Type"))
	if err != nil {
		return ociTree{}, fmt.Errorf("%s: %w", ref, err)
	}

	blobURL := "https://" + ref.Host + "/v2/" + ref.Path + "/blobs/" + layer.Digest
	blob, _, err := s.get(ctx, blobURL, "", maxLayer, true)
	if err != nil {
		return ociTree{}, err
	}
	if int64(len(blob)) != layer.Size {
		return ociTree{}, fmt.Errorf("%w: %s: the layer is %d bytes and the manifest says %d", ErrArtifactRefused, ref, len(blob), layer.Size)
	}
	blobSum := sha256.Sum256(blob)
	if "sha256:"+hex.EncodeToString(blobSum[:]) != layer.Digest {
		return ociTree{}, fmt.Errorf("%w: %s: the layer does not hash to the digest the manifest names", ErrDigestMismatch, ref)
	}
	files, err := untarCatalogue(blob)
	if err != nil {
		return ociTree{}, fmt.Errorf("%s: %w", ref, err)
	}
	return ociTree{at: time.Now(), digest: digest, files: files}, nil
}

type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// checkManifest says whether a manifest is a catalogue's, and answers its one
// layer.
func checkManifest(raw []byte, contentType string) (ociDescriptor, error) {
	refuse := func(why string) (ociDescriptor, error) {
		return ociDescriptor{}, fmt.Errorf("%w: %s", ErrArtifactRefused, why)
	}
	var m struct {
		SchemaVersion int             `json:"schemaVersion"`
		MediaType     string          `json:"mediaType"`
		ArtifactType  string          `json:"artifactType"`
		Config        ociDescriptor   `json:"config"`
		Layers        []ociDescriptor `json:"layers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return refuse("the manifest does not parse")
	}
	served, _, _ := mime.ParseMediaType(contentType)
	if m.SchemaVersion != 2 || (m.MediaType != ociManifestType && (m.MediaType != "" || served != ociManifestType)) {
		return refuse("it is not an OCI image manifest (an image index, or another kind of manifest, is not a catalog)")
	}
	if m.ArtifactType != OCIArtifactType && (m.ArtifactType != "" || m.Config.MediaType != OCIArtifactType) {
		return refuse("its artifact type is not " + OCIArtifactType)
	}
	if len(m.Layers) != 1 {
		return refuse(fmt.Sprintf("it has %d layers; a catalog is one", len(m.Layers)))
	}
	layer := m.Layers[0]
	if layer.MediaType != OCILayerType {
		return refuse("its layer is " + layer.MediaType + ", not " + OCILayerType)
	}
	if d, err := CanonicalDigest(layer.Digest); err != nil || d != layer.Digest {
		return refuse("its layer's digest is not sha256:<64 lower-case hex characters>")
	}
	if layer.Size <= 0 || layer.Size > maxLayer {
		return refuse(fmt.Sprintf("its layer is %d bytes; a catalog is at most %d", layer.Size, maxLayer))
	}
	return layer, nil
}

// untarCatalogue unpacks a catalogue's layer: a gzip'd tar of plain files
// below plain directories, each path relative and inside the catalogue.
//
// What it may not hold refuses the whole artifact rather than being skipped:
// a link of any kind, a device, a path that is absolute, climbs out with
// "..", or is written twice. A catalogue that holds one was not built by the
// tools that build catalogs, and what else it holds is then not known.
func untarCatalogue(blob []byte) (map[string][]byte, error) {
	refuse := func(why string) (map[string][]byte, error) {
		return nil, fmt.Errorf("%w: %s", ErrArtifactRefused, why)
	}
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return refuse("its layer is not gzip")
	}
	defer func() { _ = gz.Close() }()
	limited := &io.LimitedReader{R: gz, N: maxTree + 1}
	tr := tar.NewReader(limited)
	files := map[string][]byte{}
	seen := map[string]bool{}
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if limited.N <= 0 {
			return refuse(fmt.Sprintf("it unpacks to more than %d bytes", maxTree))
		}
		if err != nil {
			return refuse("its layer is not a tar: " + err.Error())
		}
		if entries >= maxEntries {
			return refuse(fmt.Sprintf("it holds more than %d entries", maxEntries))
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// Comments a tool writes for the whole archive (git archive
			// does); no file.
			continue
		}
		name, err := catalogueEntryName(hdr.Name)
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if name != "" {
				seen[name+"/"] = true
			}
			continue
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is what older tools write for a file
		default:
			return refuse(fmt.Sprintf("%s is not a plain file or directory (a link, a device or a pipe is not part of a catalog)", hdr.Name))
		}
		if name == "" || seen[name] || seen[name+"/"] {
			return refuse(fmt.Sprintf("%s is written twice, or is not a file's name", hdr.Name))
		}
		seen[name] = true
		if hdr.Size > maxBundle {
			return refuse(fmt.Sprintf("%s is %d bytes; a file of a catalog is at most %d", name, hdr.Size, maxBundle))
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxBundle+1))
		if err != nil {
			if limited.N <= 0 {
				return refuse(fmt.Sprintf("it unpacks to more than %d bytes", maxTree))
			}
			return refuse("reading " + name + ": " + err.Error())
		}
		if len(body) > maxBundle {
			return refuse(fmt.Sprintf("%s is larger than %d bytes", name, maxBundle))
		}
		files[name] = body
	}
	if limited.N <= 0 {
		return refuse(fmt.Sprintf("it unpacks to more than %d bytes", maxTree))
	}
	return files, nil
}

// catalogueEntryName is an entry's path as the catalogue is read by it, or
// why it is refused. "./index.yaml" is "index.yaml"; "." and "./" are the
// catalogue itself, "".
func catalogueEntryName(raw string) (string, error) {
	refuse := func(why string) (string, error) {
		return "", fmt.Errorf("%w: the entry %q %s", ErrArtifactRefused, raw, why)
	}
	if strings.ContainsAny(raw, "\\\x00") {
		return refuse("is not a plain path")
	}
	if strings.HasPrefix(raw, "/") {
		return refuse("is an absolute path")
	}
	name := strings.TrimSuffix(raw, "/")
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if name == "." || name == "" {
		return "", nil
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return refuse("climbs out of the catalog")
		}
		if part == "" || part == "." {
			return refuse("is not a plain path")
		}
	}
	if path.Clean(name) != name {
		return refuse("is not a plain path")
	}
	return name, nil
}

// ── The registry's protocol ─────────────────────────────────────────────────

// registrySession is one pull: the requests to one registry for one artifact,
// and the authorization the registry asked for.
type registrySession struct {
	client        *http.Client
	ref           OCIReference
	reg           Registry
	authorization string
	authenticated bool
}

// get is one request to the registry, at most limit bytes of its answer.
// A blob may be redirected to the host that serves it (followRedirect); a
// manifest may not.
func (s *registrySession) get(ctx context.Context, address, accept string, limit int, followRedirect bool) ([]byte, http.Header, error) {
	resp, err := s.request(ctx, address, accept, s.authorization)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && !s.authenticated {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		if err := s.authenticate(ctx, challenge); err != nil {
			return nil, nil, err
		}
		resp, err = s.request(ctx, address, accept, s.authorization)
		if err != nil {
			return nil, nil, err
		}
	}
	for hop := 0; followRedirect && isRedirect(resp.StatusCode); hop++ {
		// A registry serves a blob from where it keeps blobs: ghcr.io
		// answers every blob with a redirect to a signed address of its
		// storage. Followed, because a registry cannot otherwise be read --
		// and safe to follow, because nothing is trusted that arrives this
		// way: the bytes are held to the digest the manifest names, the
		// host is held to every rule a catalogue's address is held to, and
		// the request carries no credential. The signed address in the
		// redirect is what admits it.
		location := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if hop >= maxRedirects {
			return nil, nil, fmt.Errorf("catalogue: %s: the layer is redirected more than %d times", s.ref.Host, maxRedirects)
		}
		next, err := redirectTarget(resp.Request.URL, location)
		if err != nil {
			return nil, nil, err
		}
		resp, err = s.request(ctx, next, "", "")
		if err != nil {
			return nil, nil, err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	host := resp.Request.URL.Host
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		// Not fs.ErrNotExist: that is a file the catalogue does not have,
		// and a catalogue that is not there is not one without an index.
		return nil, nil, fmt.Errorf("catalogue: %s has no artifact %s (or does not show it to this credential)", host, s.ref)
	case isRedirect(resp.StatusCode):
		return nil, nil, fmt.Errorf("catalogue: %s redirects elsewhere, and a redirect is not followed for a manifest: "+
			"name the registry that serves the artifact", host)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, nil, fmt.Errorf("catalogue: %s refused %s (%d): the repository's credential does not give access to it, or is not set",
			host, s.ref, resp.StatusCode)
	default:
		return nil, nil, fmt.Errorf("catalogue: %s answered %d", host, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, nil, fmt.Errorf("catalogue: reading from %s: %w", host, err)
	}
	if len(body) > limit {
		return nil, nil, fmt.Errorf("%w: %s serves more than %d bytes for %s", ErrArtifactRefused, host, limit, s.ref)
	}
	return body, resp.Header, nil
}

func isRedirect(code int) bool { return code >= 300 && code < 400 }

func (s *registrySession) request(ctx context.Context, address, accept, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalogue: %s: %w", req.URL.Host, err)
	}
	return resp, nil
}

// redirectTarget is where a blob's redirect points, if it may be followed
// there: https, on 443, at a public host, with nothing in it that would be a
// credential. The dialer checks what the host resolves to, as for any
// catalogue.
func redirectTarget(from *url.URL, location string) (string, error) {
	next, err := from.Parse(location)
	if err != nil || location == "" {
		return "", fmt.Errorf("catalogue: %s redirects to an address that does not parse", from.Host)
	}
	if next.User != nil {
		return "", fmt.Errorf("%w: %s redirects to an address with a user name or password", ErrAddressRefused, from.Host)
	}
	if err := CheckAddress(next.Scheme + "://" + next.Host + "/"); err != nil {
		return "", fmt.Errorf("catalogue: %s redirects the layer to a host it is not fetched from: %w", from.Host, err)
	}
	return next.String(), nil
}

// authenticate answers a registry's challenge once.
//
// Basic: the repository's login is sent to the registry itself. Bearer: a
// token is asked of the realm the registry names, with the login, for a pull
// of this one repository, and is then presented to the registry. The realm is
// held to the rules of a catalogue's address and must be of the registry's
// own registrable domain (ghcr.io's is ghcr.io; registry-1.docker.io's,
// auth.docker.io), so a registry cannot have the login sent to somebody else.
func (s *registrySession) authenticate(ctx context.Context, challenge string) error {
	s.authenticated = true
	scheme, params := parseChallenge(challenge)
	host := s.ref.Host
	switch scheme {
	case "basic":
		if s.reg.Username == "" && s.reg.Password == "" {
			return fmt.Errorf("catalogue: %s asks for a login, and the repository declares no credential", host)
		}
		if s.reg.Token != "" {
			return fmt.Errorf("catalogue: %s asks for a user name and a password, and the repository's credential is a token", host)
		}
		s.authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(s.reg.Username+":"+s.reg.Password))
		return nil
	case "bearer":
	default:
		return fmt.Errorf("catalogue: %s refused the request, and asks for no authentication a catalog is read with", host)
	}
	if s.reg.Token != "" {
		// The token was presented and refused. It is not a login, so it is
		// not traded at the realm.
		return fmt.Errorf("catalogue: %s refused the repository's token; a registry that issues its own tokens is read with a user name and a password", host)
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || params["realm"] == "" || realm.User != nil {
		return fmt.Errorf("catalogue: %s names no token realm that can be asked", host)
	}
	if err := CheckAddress(realm.Scheme + "://" + realm.Host + "/"); err != nil {
		return fmt.Errorf("catalogue: the token realm of %s is not asked: %w", host, err)
	}
	if !sameRegistrableDomain(realm.Hostname(), host) {
		return fmt.Errorf("%w: the token realm of %s is %s, of another domain; the repository's credential is sent to its own registry only",
			ErrRegistryRefused, host, realm.Hostname())
	}
	q := realm.Query()
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	// Pull, of this repository: whatever the challenge's scope says. What
	// the token is good for is the director's to ask, not the registry's.
	q.Set("scope", "repository:"+s.ref.Path+":pull")
	realm.RawQuery = q.Encode()
	authorization := ""
	if s.reg.Username != "" || s.reg.Password != "" {
		authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(s.reg.Username+":"+s.reg.Password))
	}
	resp, err := s.request(ctx, realm.String(), "application/json", authorization)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("catalogue: the token realm of %s answered %d: the repository's credential was not accepted", host, resp.StatusCode)
	}
	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenAnswer+1))
	if err != nil || len(raw) > maxTokenAnswer || json.Unmarshal(raw, &answer) != nil {
		return fmt.Errorf("catalogue: the token realm of %s answered something that is not a token", host)
	}
	token := answer.Token
	if token == "" {
		token = answer.AccessToken
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return fmt.Errorf("catalogue: the token realm of %s answered no token", host)
	}
	s.authorization = "Bearer " + token
	return nil
}

// sameRegistrableDomain says whether two hosts are of one registrable domain:
// the public suffix and the one label in front of it.
func sameRegistrableDomain(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return true
	}
	da, errA := publicsuffix.EffectiveTLDPlusOne(a)
	db, errB := publicsuffix.EffectiveTLDPlusOne(b)
	return errA == nil && errB == nil && da == db
}

// parseChallenge reads a WWW-Authenticate header: its scheme, lower case, and
// its parameters.
func parseChallenge(header string) (string, map[string]string) {
	header = strings.TrimSpace(header)
	scheme, rest, _ := strings.Cut(header, " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, found := strings.Cut(rest, "=")
		if !found {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		after = strings.TrimSpace(after)
		var value string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			value, after = after[1:1+end], after[2+end:]
		} else {
			value, after, _ = strings.Cut(after, ",")
			after = "," + after
		}
		params[key] = value
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(after), ","))
	}
	return strings.ToLower(scheme), params
}

// ── The repository's credential ─────────────────────────────────────────────

// DeclaredRegistry is a repository as its declaration in git states it.
type DeclaredRegistry struct {
	// URL is its declared address, oci://<host>[/<path>].
	URL string
	// VaultPath is where its credential is kept; "" for a repository that
	// declares none.
	VaultPath string
	// Bearer says the credential is one token rather than a login.
	Bearer bool
}

// VaultRegistries answers a repository from its declaration and its
// credential from the vault.
//
// Declared reads the declaration, and answers only a repository owner
// declared: a tenant's own, or the cluster's. Read reads one path of the
// vault. The path is the declaration's, and is held here as well to the one
// place the owner's repository credentials are kept, so that neither a
// declaration nor a mistake in reading one can make a catalogue of one
// tenant be read with another's credential, or with the cluster's.
type VaultRegistries struct {
	Declared func(ctx context.Context, owner, repository string) (DeclaredRegistry, error)
	Read     func(ctx context.Context, path string) (map[string]string, error)
}

// RepositoryCredentialPrefix is where an owner's repository credentials are
// kept in the vault: a tenant's, or the cluster's for "".
func RepositoryCredentialPrefix(owner string) string {
	if owner == "" {
		return "gentian-os/kernel/repositories/"
	}
	return "gentian-os/tenants/" + owner + "/repositories/"
}

// Registry implements Registries.
func (v VaultRegistries) Registry(ctx context.Context, owner, repository string) (Registry, error) {
	if v.Declared == nil {
		return Registry{}, fmt.Errorf("%w: this director reads no repository declarations", ErrRegistryRefused)
	}
	d, err := v.Declared(ctx, owner, repository)
	if err != nil {
		return Registry{}, err
	}
	_, prefix, err := registryPrefix(d.URL)
	if err != nil {
		return Registry{}, err
	}
	out := Registry{Prefix: prefix}
	if d.VaultPath == "" {
		return out, nil
	}
	if d.VaultPath != RepositoryCredentialPrefix(owner)+repository {
		return Registry{}, fmt.Errorf("%w: the credential of %s is declared at %s, which is not where %s keeps it",
			ErrRegistryRefused, repository, d.VaultPath, ownerWords(owner))
	}
	if v.Read == nil {
		return Registry{}, fmt.Errorf("%w: this director reads no repository credential, and %s declares one", ErrRegistryRefused, repository)
	}
	data, err := v.Read(ctx, d.VaultPath)
	if err != nil {
		return Registry{}, fmt.Errorf("catalogue: the credential of repository %s could not be read (is repository-%s set?): %w",
			repository, repository, err)
	}
	if d.Bearer || data["username"] == "" {
		out.Token = data["password"]
	} else {
		out.Username, out.Password = data["username"], data["password"]
	}
	if out.Token == "" && out.Password == "" {
		return Registry{}, fmt.Errorf("catalogue: the credential of repository %s is not set (repository-%s)", repository, repository)
	}
	return out, nil
}

func ownerWords(owner string) string {
	if owner == "" {
		return "the cluster"
	}
	return "tenant " + owner
}
