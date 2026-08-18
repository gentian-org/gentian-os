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

package credentialmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gentian-org/gentian-os/internal/handover"
)

var externalSecretGVK = schema.GroupVersionKind{
	Group:   "external-secrets.io",
	Version: "v1",
	Kind:    "ExternalSecret",
}

// Server exposes the Credential Manager API.
//
// It has no token field. That is not an oversight: every write takes the
// caller's token, so there is no service authority for a bug to reach for.
type Server struct {
	Addr      string
	Catalogue *Catalogue
	Bao       *OpenBao
	Validator Validator

	// ClusterAdminPolicy is the OpenBao policy whose presence on an exchanged
	// token means the caller may see cluster-scoped requirements.
	ClusterAdminPolicy string
	// TenantClaimKey is the auth.metadata key the role maps the tenant into.
	TenantClaimKey string

	// Client and HandoverNamespace are how a successful exchange becomes a
	// fact the rest of the cluster can read. See internal/handover: this
	// service performs the only OIDC token exchange that happens anywhere, so
	// it is the only thing in a position to observe that the human write path
	// works. Nil Client disables recording rather than failing requests.
	Client            client.Client
	HandoverNamespace string

	// now is injected so the recorder can be tested without waiting for a
	// clock. Nil means time.Now.
	now func() time.Time
}

// Validator checks a credential against its target before it is stored. The
// interface is here rather than a concrete type so the API layer cannot be
// tempted to skip it — a nil Validator is a programming error, not a mode.
type Validator interface {
	Validate(ctx context.Context, kind string, fields map[string]string) error
}

// Routes returns the mux. Exported so the route-enumeration test can walk every
// endpoint and assert none of them can return a credential value.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /v1/credentials", s.handleList)
	mux.HandleFunc("GET /v1/credentials/{name}", s.handleGet)
	mux.HandleFunc("PUT /v1/credentials/{name}", s.handleSet)
	mux.HandleFunc("GET /v1/repositories", s.handleListRepositories)
	mux.HandleFunc("PUT /v1/repositories/{name}", s.handleSetRepository)
	mux.HandleFunc("DELETE /v1/repositories/{name}", s.handleDeleteRepository)
	return mux
}

// Start satisfies manager.Runnable so this rides the operator's manager rather
// than being a second Deployment to secure, schedule and upgrade.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// NewRunnableFromEnv wires the server from the operator's environment.
func NewRunnableFromEnv(mgr manager.Manager, validator Validator) (*Server, error) {
	addr := envOr("CREDENTIAL_MANAGER_ADDR", ":9444")
	baoAddr := os.Getenv("BAO_ADDR")
	if baoAddr == "" {
		return nil, fmt.Errorf("BAO_ADDR is required for the credential manager")
	}
	if validator == nil {
		return nil, fmt.Errorf("a validator is required: storing an unvalidated credential is what this service exists to prevent")
	}
	return &Server{
		Addr: addr,
		Catalogue: &Catalogue{
			Client:         mgr.GetClient(),
			ProbeNamespace: envOr("CREDENTIAL_PROBE_NAMESPACE", "gentian-system"),
		},
		Bao: NewOpenBao(
			baoAddr,
			envOr("BAO_KV_MOUNT", "secret"),
			// The backend is enabled at -path=oidc, so this is the mount, not
			// the plugin's default "jwt" name.
			envOr("BAO_AUTH_MOUNT", "oidc"),
			// JWT-typed roles, not the oidc-typed ones behind the browser flow:
			// a role with role_type oidc refuses a direct token exchange.
			splitList(envOr("BAO_OIDC_ROLES", "cluster-admin-jwt,tenant-admin-jwt")),
		),
		Validator:          validator,
		ClusterAdminPolicy: envOr("CREDENTIAL_CLUSTER_ADMIN_POLICY", "cluster-admin"),
		TenantClaimKey:     envOr("CREDENTIAL_TENANT_CLAIM", "tenant"),
		Client:             mgr.GetClient(),
		// The operator's own namespace by default: the record belongs beside
		// the thing that gates on it, not beside the credentials.
		HandoverNamespace: envOr("HANDOVER_NAMESPACE", envOr("OPERATOR_NAMESPACE", "gentian-system")),
	}, nil
}

// splitList parses a comma-separated env value, dropping blanks so a trailing
// comma or an accidental double one does not become a role named "".
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// caller carries the authenticated identity for one request.
type caller struct {
	// bao is the exchanged OpenBao token and the identity that came with it.
	bao OpenBaoIdentity
	// name is the human recorded as having set a credential.
	name string
	// view is what this caller may see, derived from bao — never from the
	// request.
	view Viewer
}

// OpenBaoIdentity is aliased so the handler signature reads as identity rather
// than as a token string, which is what it used to be.
type OpenBaoIdentity = Identity

// bearer pulls the OIDC token out of the request. It performs no authorisation:
// that is what the exchange is for.
func bearer(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", fmt.Errorf("a bearer token is required")
	}
	tok := strings.TrimPrefix(auth, "Bearer ")
	if tok == "" {
		return "", fmt.Errorf("empty bearer token")
	}
	return tok, nil
}

// identify establishes who the caller is by asking OpenBao.
//
// Scope and tenant used to be read from a query parameter and a header, which
// made them claims the caller made about itself: appending ?scope=cluster
// widened the listing, and X-Gentian-User named whoever you liked in the audit
// metadata. That was survivable only while every caller was already a cluster
// admin. A tenant admin holding a token makes it a disclosure.
//
// So identity is now OpenBao's verdict. The exchange verifies the JWT's
// signature, issuer and audience and applies the role's bound claims; this
// service reads the result and never parses the token itself. Being a second
// identity authority is precisely how the two come to disagree.
//
// The cost is that a listing now requires OpenBao to be reachable, where before
// it degraded to a catalogue with no metadata. That is the right trade: an
// unauthorised listing is not a degraded listing.
//
// The token is never logged and never stored. It lives for the duration of one
// request, which is the longest a credential that can write every secret in the
// cluster should exist anywhere in this process.
func (s *Server) identify(ctx context.Context, r *http.Request) (caller, error) {
	tok, err := bearer(r)
	if err != nil {
		return caller{}, err
	}
	id, err := s.Bao.ExchangeToken(ctx, tok)
	if err != nil {
		return caller{}, err
	}

	c := caller{bao: id, view: s.viewerFor(id)}
	// The username comes from the role's user_claim, so it is the verified
	// subject rather than a header. Falling back to the claim-mapped name keeps
	// this working across OpenBao versions that report it differently.
	for _, k := range []string{"username", "preferred_username", "user"} {
		if v := id.Metadata[k]; v != "" {
			c.name = v
			break
		}
	}

	// The exchange above is the only proof that exists that a human can write
	// to OpenBao at all — every other check in the installer establishes that
	// the parts are present, not that they open. Recorded here, at the one
	// point every handler passes through, so no future endpoint can be added
	// that authenticates without proving.
	s.recordHandover(ctx, c)
	return c, nil
}

// recordHandover notes a successful cluster-admin exchange, best effort.
//
// Never fails the request. A caller who has just authenticated should not be
// refused because a ConfigMap write lost a conflict — and the next request
// records it anyway. The inverse would be worse than the gap it closes: the
// service would deny writes when the cluster is healthy and the recorder is not.
func (s *Server) recordHandover(ctx context.Context, c caller) {
	if s.Client == nil || s.HandoverNamespace == "" {
		return
	}
	// Cluster admin only. A tenant admin's exchange proves their own role
	// opens, which is worth having but is not the credential the bootstrap
	// token is being traded for.
	if !c.view.ClusterAdmin {
		return
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if err := handover.RecordWritePathProven(ctx, s.Client, s.HandoverNamespace, c.name, now()); err != nil {
		ctrl.Log.WithName("credentialmgr").Error(err, "recording the handover proof")
	}
}

// viewerFor turns OpenBao's verdict into a visibility decision.
//
// Cluster admin is recognised by the policy OpenBao attached, not by a group
// name this service would have to keep in step with Keycloak. The tenant comes
// from the role's claim mappings, so a role that maps no tenant yields a viewer
// that sees no tenant-scoped requirement — closed by default.
func (s *Server) viewerFor(id Identity) Viewer {
	v := Viewer{Tenant: id.Metadata[s.TenantClaimKey]}
	for _, p := range id.Policies {
		if p == s.ClusterAdminPolicy {
			v.ClusterAdmin = true
			break
		}
	}
	return v
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	items, err := s.Catalogue.List(r.Context(), c.view)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.decorate(r.Context(), c, items)
	writeJSON(w, http.StatusOK, map[string]any{"credentials": items})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	item, err := s.Catalogue.Get(r.Context(), r.PathValue("name"), c.view)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	one := []Status{*item}
	s.decorate(r.Context(), c, one)
	writeJSON(w, http.StatusOK, one[0])
}

// decorate adds "who set it and when" from OpenBao's metadata endpoint.
//
// Best-effort on purpose: a caller whose policy cannot read metadata still gets
// the catalogue and ESO's satisfaction verdict, which is the useful part. A
// failure here must not turn a working list into an error page.
func (s *Server) decorate(ctx context.Context, c caller, items []Status) {
	for i := range items {
		md, err := s.Bao.Metadata(ctx, c.bao.Token, items[i].VaultPath)
		if err != nil {
			continue
		}
		items[i].SetBy = md.SetBy
		if !md.UpdatedAt.IsZero() {
			items[i].UpdatedAt = md.UpdatedAt.Format(time.RFC3339)
		}
	}
}

// setRequest is the write body. Fields carries values IN; nothing carries them
// back out.
type setRequest struct {
	Fields map[string]string `json:"fields"`
}

func (s *Server) handleSet(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	name := r.PathValue("name")
	req, err := s.Catalogue.Get(r.Context(), name, c.view)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}

	var body setRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("malformed body: %w", err))
		return
	}
	if err := checkFields(req, body.Fields); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Validate BEFORE storing. This is what justifies the service existing at
	// all: it turns "tenant provisioning stalled because a password was pasted
	// with a trailing newline" into a rejected form field.
	if req.Validator != "" && req.Validator != "noop" {
		if err := s.Validator.Validate(r.Context(), req.Validator, body.Fields); err != nil {
			writeErr(w, http.StatusUnprocessableEntity,
				fmt.Errorf("validation failed against the target endpoint: %w", err))
			return
		}
	}

	// The caller's token performs the write. If the caller's policy forbids the
	// path, nothing is stored — the service has no authority to fall back on.
	if err := s.Bao.Write(r.Context(), c.bao.Token, req.VaultPath, body.Fields, c.name); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}

	// Metadata only in the response, as everywhere else.
	writeJSON(w, http.StatusOK, map[string]any{
		"name":      name,
		"vaultPath": req.VaultPath,
		"stored":    true,
		"setBy":     c.name,
	})
}

// checkFields enforces the declared schema before anything is sent anywhere.
func checkFields(req *Status, got map[string]string) error {
	if len(got) == 0 {
		return fmt.Errorf("no fields supplied")
	}
	declared := map[string]Field{}
	for _, f := range req.Fields {
		declared[f.Key] = f
	}
	for k := range got {
		if _, ok := declared[k]; !ok {
			return fmt.Errorf("unknown field %q for requirement %q", k, req.Name)
		}
	}
	for _, f := range req.Fields {
		v, present := got[f.Key]
		if !present {
			return fmt.Errorf("missing field %q", f.Key)
		}
		if strings.TrimSpace(v) != v {
			// The single most common way a pasted credential breaks.
			return fmt.Errorf("field %q has leading or trailing whitespace", f.Key)
		}
		if f.MinLength > 0 && len(v) < f.MinLength {
			return fmt.Errorf("field %q must be at least %d characters", f.Key, f.MinLength)
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		ctrl.Log.WithName("credentialmgr").Error(err, "encoding response")
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
