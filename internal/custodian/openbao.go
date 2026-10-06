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

// Package custodian serves the custodian: a view over the
// CredentialRequirement catalogue and ESO's satisfaction status, plus the one
// write path by which a person sets a credential.
//
// It follows the director's pattern. The director holds the credential to
// push to git, asks the authorization store whether a person may make a
// change, and then makes it under its own name, recording whose intent it
// was. The custodian holds an identity at the vault, asks the same store
// whether a person may set a credential, and then sets it under its own
// name, recording who set it. A secret is the one thing that must never be a
// commit, so it has a keeper of its own; in every other respect the two are
// the same shape.
//
// Three constraints shape the package.
//
// # The store decides, and nothing else does
//
// Who may see that a credential is required and who may set it are the
// authorization store's answers (authorize.go). The vault is not asked who
// the caller is and is never shown the caller's token: an earlier design
// exchanged that token at the vault and took the vault's verdict, which made
// a group written into a token a second source of rights.
//
// # Its identity at the vault cannot read a value
//
// The custodian logs in as itself, by its ServiceAccount, to a role whose
// policy lets it write a credential and read and annotate its metadata, and
// nothing else. It is given no capability to read a stored value, so a fault
// in this service cannot disclose one.
//
// # Write-only, no read-back
//
// Displaying a credential creates an exfiltration surface, needs a different
// threat model, and hands an attacker with a stolen session everything at once.
// The API returns metadata only: whether a value exists, who set it and when,
// and the last validation result. Lost credentials are rotated, not recovered.
//
// Enforced by [Status] having no field capable of carrying a value, and by a
// test that enumerates every route and asserts none returns one.
package custodian

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// OpenBao is a minimal client. It deliberately implements only the calls this
// service needs; a fuller client would make it easy to add a read path that
// the design forbids.
type OpenBao struct {
	Addr    string
	KVMount string

	// KubernetesMount is where the Kubernetes auth backend is enabled, and
	// Role the role the custodian logs in to with its ServiceAccount token.
	// The role's policy is what bounds the custodian: write and metadata on
	// credential paths, and no reading of values.
	KubernetesMount string
	Role            string
	// ServiceAccountTokenPath is the projected token this pod presents.
	ServiceAccountTokenPath string

	HTTP *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// ErrUpstream marks a failure to REACH OpenBao, as opposed to OpenBao
// declining a request.
//
// The distinction is the whole reason this exists. Every transport failure used
// to arrive at the handler as an ordinary error and leave as 401, so the portal
// told an administrator "OpenBao refused the token — check that you are in the
// cluster-admin group" when the truth was that no request had reached OpenBao
// at all. The advice was correct, actionable, and about the wrong thing, which
// is worse than no advice: it sends someone to audit group membership that is
// already right.
var ErrUpstream = errors.New("openbao unreachable")

// NewOpenBao builds a client with a bounded HTTP timeout, so a hung OpenBao
// surfaces as a failed request rather than a wedged handler.
//
// caCert, when non-empty, is the PEM OpenBao's certificate is verified against.
// It is not optional in practice: OpenBao serves a self-signed certificate on
// this platform, so the default transport — which verifies against the system
// roots — fails every exchange. ESO reaches the same endpoint by loading the
// same CA out of the openbao-tls Secret, and this is that pattern in Go.
//
// An empty caCert keeps the system roots, which is right for a cluster that
// gave OpenBao a publicly trusted certificate.
func NewOpenBao(addr, kvMount, role string, caCert []byte, skipVerify bool) *OpenBao {
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if skipVerify {
		// An escape hatch, not a mode. Named so it appears in the Deployment
		// for anyone wondering why verification is not happening.
		tlsConf.InsecureSkipVerify = true
	} else if len(caCert) > 0 {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caCert) {
			tlsConf.RootCAs = pool
		} else {
			ctrl.Log.WithName("custodian").Info(
				"the configured OpenBao CA is not valid PEM; falling back to the system roots")
		}
	}
	return &OpenBao{
		Addr:                    strings.TrimSuffix(addr, "/"),
		KVMount:                 kvMount,
		KubernetesMount:         "kubernetes",
		Role:                    role,
		ServiceAccountTokenPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		HTTP: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsConf},
		},
	}
}

// ErrVaultLogin is the custodian failing to log in to the vault as itself.
// It is a fault of the installation -- the role, its policy, or the binding to
// this ServiceAccount -- and never of the person asking.
var ErrVaultLogin = errors.New("the custodian could not log in to OpenBao")

// Token returns the custodian's own token at the vault, logging in with its
// ServiceAccount when it has none or the one it has is about to expire.
//
// This is the only token the custodian ever presents to the vault. A caller's
// token is verified here and goes no further.
func (b *OpenBao) Token(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && (b.tokenExp.IsZero() || time.Until(b.tokenExp) > 60*time.Second) {
		return b.token, nil
	}
	jwt, err := os.ReadFile(b.ServiceAccountTokenPath)
	if err != nil {
		return "", fmt.Errorf("%w: reading the ServiceAccount token: %v", ErrVaultLogin, err)
	}
	body, _ := json.Marshal(map[string]string{"role": b.Role, "jwt": strings.TrimSpace(string(jwt))})
	url := fmt.Sprintf("%s/v1/auth/%s/login", b.Addr, b.KubernetesMount)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// OpenBao's own words go to the log and not to the caller: they can
		// name roles and policies, and the caller can do nothing about them.
		detail := strings.TrimSpace(readCapped(resp.Body, 512))
		ctrl.Log.WithName("custodian").Info("OpenBao refused the custodian's login",
			"role", b.Role, "mount", b.KubernetesMount, "status", resp.StatusCode, "openbao", detail)
		return "", fmt.Errorf("%w: role %q answered HTTP %d", ErrVaultLogin, b.Role, resp.StatusCode)
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Auth.ClientToken == "" {
		return "", fmt.Errorf("%w: the login answer carried no token", ErrVaultLogin)
	}
	b.token = out.Auth.ClientToken
	b.tokenExp = time.Now().Add(time.Hour)
	if out.Auth.LeaseDuration > 0 {
		b.tokenExp = time.Now().Add(time.Duration(out.Auth.LeaseDuration) * time.Second)
	}
	return b.token, nil
}

// forget drops the cached token, so the next call logs in again. Called when
// the vault refuses a token it issued: it was revoked, or the vault restarted.
func (b *OpenBao) forget() {
	b.mu.Lock()
	b.token, b.tokenExp = "", time.Time{}
	b.mu.Unlock()
}

// SetStaticToken makes Token return tok without logging in. For tests against
// a stand-in vault.
func (b *OpenBao) SetStaticToken(tok string) {
	b.mu.Lock()
	b.token, b.tokenExp = tok, time.Time{}
	b.mu.Unlock()
}

// PathMetadata is what the API is allowed to say about a stored credential.
// There is no field here that can carry a value, and that is the point.
type PathMetadata struct {
	Exists bool `json:"exists"`
	// Recipient is an age public key recorded alongside a stored identity.
	// Metadata rather than data, deliberately: a public key is not a secret,
	// and keeping it here means asking "which key is escrowed" never reads the
	// private half at all.
	Recipient string    `json:"recipient,omitempty"`
	SetBy     string    `json:"setBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	Version   int       `json:"version,omitempty"`
}

// Metadata reads a path's metadata. It calls the KV *metadata* endpoint, never
// the data endpoint, so no value is retrievable through this client at all.
func (b *OpenBao) Metadata(ctx context.Context, token, path string) (PathMetadata, error) {
	url := fmt.Sprintf("%s/v1/%s/metadata/%s", b.Addr, b.KVMount, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PathMetadata{}, err
	}
	req.Header.Set("X-Vault-Token", token)

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return PathMetadata{}, fmt.Errorf("openbao unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return PathMetadata{Exists: false}, nil
	case http.StatusOK:
	default:
		return PathMetadata{}, fmt.Errorf("metadata read failed (HTTP %d)", resp.StatusCode)
	}

	var out struct {
		Data struct {
			CurrentVersion int    `json:"current_version"`
			UpdatedTime    string `json:"updated_time"`
			CustomMetadata struct {
				SetBy     string `json:"set_by"`
				Recipient string `json:"recipient"`
			} `json:"custom_metadata"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PathMetadata{}, err
	}
	md := PathMetadata{
		Exists:    true,
		Version:   out.Data.CurrentVersion,
		SetBy:     out.Data.CustomMetadata.SetBy,
		Recipient: out.Data.CustomMetadata.Recipient,
	}
	if t, err := time.Parse(time.RFC3339, out.Data.UpdatedTime); err == nil {
		md.UpdatedAt = t
	}
	return md, nil
}

// Write stores a credential with the token given, which is the custodian's own.
//
// The token is a parameter rather than client state precisely so this cannot be
// called on the service's own authority — there is no service authority to call
// it with.
//
// set_by is recorded as custom metadata so the "who set this" the API reports
// survives independently of the audit device, which an operator reading the UI
// may not have access to.
func (b *OpenBao) Write(ctx context.Context, token, path string, fields map[string]string, setBy string) error {
	return b.WriteWithMetadata(ctx, token, path, fields, setBy, nil)
}

// WriteWithMetadata stores a value and records extra custom metadata beside it.
// Used for the escrowed backup key, whose public half belongs in metadata so
// that reading it never touches the private one.
func (b *OpenBao) WriteWithMetadata(ctx context.Context, token, path string, fields map[string]string, setBy string, extra map[string]string) error {
	if token == "" {
		return fmt.Errorf("no caller token: the custodian cannot write on its own authority")
	}
	// No options block, and specifically no check-and-set.
	//
	// This sent {"options":{"cas":null}}. cas is KV v2's check-and-set: a
	// value of 0 means "write only if this path does not exist", and a null
	// is not a way of saying "no opinion" — it is a cas the server has to
	// parse, which it either rejects or reads as zero. Either way the write
	// fails the moment the path already has a version.
	//
	// Every path here has one. The installer seeds this mount at bootstrap,
	// so supplying a credential through the custodian is always an
	// UPDATE of a path that exists, never a create — which made the one
	// operation this service exists to perform the one it could never do.
	//
	// Omitting options entirely is an unconditional write, which is what
	// "supply this credential" means. The mount does not set cas_required,
	// so nothing is being bypassed.
	// PATCH, not POST: a KV v2 write REPLACES the document, and several paths
	// hold more than the one credential a form supplies.
	//
	// gentian-os/kernel/dns/cloudflare is the case that found this. The
	// installer seeds api-token, zone-id and tunnel-cname there as one
	// document; the catalogue declares only api-token as an operator-supplied
	// field, because the other two are derived from the token and the running
	// cloudflared rather than typed by anyone. Supplying a rotated token
	// through the console therefore wrote a document containing api-token
	// alone, and the other two keys ceased to exist.
	//
	// Nothing said so. The write succeeded and the console reported success;
	// the ExternalSecret that reads all three failed two components away with
	// `cannot find secret data for key: "zone-id"`, kept serving the Secret's
	// last good content — the superseded token — and the operator went on
	// authenticating with a credential that had already been revoked.
	//
	// A merge patch is a server-side merge: keys present in the request are
	// written, keys absent are left alone, which is what "supply this
	// credential" has always meant. Doing it in this client instead would mean
	// reading the document first, and this client deliberately cannot read
	// values at all — see Metadata.
	payload := map[string]any{
		"data": fields,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/v1/%s/data/%s", b.Addr, b.KVMount, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/merge-patch+json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A patch needs something to merge onto. 404 is a path with no version
	// yet — a credential nothing has seeded — where a full write is both
	// correct and the only option, and destroys nothing because there is
	// nothing there.
	if resp.StatusCode == http.StatusNotFound {
		return b.createAndRecord(ctx, token, path, url, body, setBy, extra)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		// OpenBao's own words, not a guess at them. "the caller's policy may
		// not permit this" was a plausible reading of every non-2xx, and it
		// sent an operator to audit a policy that already granted the path
		// while the actual answer was in a response body nobody read.
		//
		// Safe to relay: reaching here means the caller already authenticated
		// and holds the cluster-admin policy, so the path and the reason are
		// things they may see. The value is not in the response.
		detail := strings.TrimSpace(readCapped(resp.Body, 2048))
		if detail == "" {
			detail = "no detail returned"
		}
		return fmt.Errorf("openbao rejected the write to %s (HTTP %d): %s", path, resp.StatusCode, detail)
	}
	return b.setCustomMetadata(ctx, token, path, setBy, extra)
}

// createAndRecord writes the whole document, for a path that does not exist yet.
//
// Only reachable from Write's 404 branch. It is the pre-patch behaviour, kept
// for the one case where replacing the document cannot lose anything: there is
// no document.
func (b *OpenBao) createAndRecord(ctx context.Context, token, path, url string, body []byte, setBy string, extra map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		detail := strings.TrimSpace(readCapped(resp.Body, 2048))
		if detail == "" {
			detail = "no detail returned"
		}
		return fmt.Errorf("openbao rejected the write to %s (HTTP %d): %s", path, resp.StatusCode, detail)
	}
	return b.setCustomMetadata(ctx, token, path, setBy, extra)
}

func (b *OpenBao) setCustomMetadata(ctx context.Context, token, path, setBy string, extra map[string]string) error {
	meta := map[string]string{}
	for k, v := range extra {
		if v != "" {
			meta[k] = v
		}
	}
	if setBy != "" {
		meta["set_by"] = setBy
	}
	if len(meta) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"custom_metadata": meta})
	url := fmt.Sprintf("%s/v1/%s/metadata/%s", b.Addr, b.KVMount, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		// The value is already stored; failing the whole request here would
		// tell the operator the write failed when it did not.
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}

// readCapped reads at most n bytes, so a large or hostile error body cannot
// become the log line.
func readCapped(r io.Reader, n int64) string {
	b, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return ""
	}
	return string(b)
}
