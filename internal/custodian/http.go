/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/handover"
	"github.com/gentian-org/gentian-os/internal/layout"
)

var externalSecretGVK = schema.GroupVersionKind{
	Group:   "external-secrets.io",
	Version: "v1",
	Kind:    "ExternalSecret",
}

// Server exposes the custodian API.
//
// It has no token field. The custodian's identity at the vault lives in the
// vault client, which logs in as the service; nothing here holds a caller's
// token past the request that brought it.
type Server struct {
	Addr      string
	Catalogue *Catalogue
	Bao       *OpenBao
	Validator Validator

	// Authz verifies the caller and asks the authorization store what they
	// may do. Required: without it every request is refused, because there is
	// then nobody to ask and nothing else may answer.
	Authz Authorizer

	// Client and HandoverNamespace are how "a person can set a credential"
	// becomes a fact the rest of the cluster can read. See internal/handover:
	// this service is the human write path, so it is the only thing in a
	// position to observe that the path works. Nil Client disables recording
	// rather than failing requests.
	Client            client.Client
	HandoverNamespace string

	// now is injected so the recorder can be tested without waiting for a
	// clock. Nil means time.Now.
	now func() time.Time
}

// Validator checks a credential against its target before it is stored. The
// interface is here rather than a concrete type so the API layer cannot be
// tempted to skip it — a nil Validator is a programming error, not a mode.
//
// host is the requirement's declared spec.validate.host, passed separately
// from fields because it is not one of the submitted credential fields — see
// EndpointValidator.Validate.
type Validator interface {
	Validate(ctx context.Context, kind, host string, fields map[string]string) error
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
	mux.HandleFunc("GET /v1/backup-identity", s.handleGetBackupIdentity)
	mux.HandleFunc("PUT /v1/backup-identity", s.handleSetBackupIdentity)
	mux.HandleFunc("GET /v1/repositories", s.handleListRepositories)
	// No PUT and no DELETE: a repository is declared and removed by a commit
	// the director makes (see repositories.go).
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
//
// graph is the authorization store the operator already holds a client for.
// It is required, like the identity provider's address: the custodian asks
// the store before every read and write, and a custodian with no store to
// ask would have to either refuse everybody or let something else decide.
func NewRunnableFromEnv(mgr manager.Manager, validator Validator, graph authz.Checker) (*Server, error) {
	addr := envOr("CUSTODIAN_ADDR", ":9444")
	if graph == nil {
		return nil, fmt.Errorf("the custodian needs the authorization store (OPENFGA_API_URL): it decides who may touch a credential")
	}
	cluster := os.Getenv("GENTIAN_DEPLOYMENTS_CLUSTER_ID")
	if cluster == "" {
		return nil, fmt.Errorf("the custodian needs GENTIAN_DEPLOYMENTS_CLUSTER_ID: the cluster is what kernel credentials are asked about")
	}
	issuer := os.Getenv("CUSTODIAN_ISSUER_BASE_URL")
	if issuer == "" {
		return nil, fmt.Errorf("the custodian needs CUSTODIAN_ISSUER_BASE_URL to verify a caller's token")
	}
	verifier, err := authn.NewVerifier(authn.Config{
		IssuerBase: issuer,
		JWKSBase:   os.Getenv("CUSTODIAN_JWKS_BASE_URL"),
		// The zone's token, minted for the director and relayed here by a
		// console as it is relayed to the director.
		Audience: envOr("CUSTODIAN_AUDIENCE", "gentian-director"),
	})
	if err != nil {
		return nil, err
	}
	authorizer := &GraphAuthorizer{
		Verifier:    verifier,
		Graph:       graph,
		Cluster:     cluster,
		KernelRealm: envOr("KERNEL_REALM", "kernel"),
		Client:      mgr.GetClient(),
	}
	baoAddr := os.Getenv("BAO_ADDR")
	if baoAddr == "" {
		return nil, fmt.Errorf("BAO_ADDR is required for the custodian")
	}
	if validator == nil {
		return nil, fmt.Errorf("a validator is required: storing an unvalidated credential is what this service exists to prevent")
	}
	// The endpoint validator needs to know where this cluster's relay is before
	// it can check a relay credential against it.
	if ev, ok := validator.(*EndpointValidator); ok && ev.Relay == nil {
		ev.Relay = clusterRelayResolver(mgr)
	}
	// And the domain a DNS credential has to have rights over. Same env the
	// operator resolves every other kernel hostname from, so the two cannot
	// disagree about which zone this cluster lives in.
	if ev, ok := validator.(*EndpointValidator); ok && ev.KernelDomain == "" {
		ev.KernelDomain = os.Getenv("KERNEL_DOMAIN")
	}
	return &Server{
		Addr: addr,
		Catalogue: &Catalogue{
			Client:         mgr.GetClient(),
			ProbeNamespace: envOr("CUSTODIAN_PROBE_NAMESPACE", layout.Namespace(layout.Control)),
		},
		Bao: NewOpenBao(
			baoAddr,
			envOr("BAO_KV_MOUNT", "secret"),
			// The custodian's own role at the vault: write and metadata on
			// credential paths, no reading of values. Bound to this pod's
			// ServiceAccount by the cluster's Composition.
			envOr("CUSTODIAN_BAO_ROLE", "gentian-os-custodian"),
			loadBaoCA(mgr),
			os.Getenv("BAO_TLS_SKIP_VERIFY") == "true",
		),
		Validator: validator,
		Authz:     authorizer,
		Client:    mgr.GetClient(),
		// The operator's own namespace by default: the record belongs beside
		// the thing that gates on it, not beside the credentials.
		HandoverNamespace: envOr("HANDOVER_NAMESPACE", envOr("OPERATOR_NAMESPACE", layout.Namespace(layout.Control))),
	}, nil
}

// The Cluster claim is read below, unstructured. Nothing registers that kind
// with the scheme, so no typed client names it and no other marker covers it —
// it needs this one or the read is refused on a real cluster.
//
// Detached, like every other marker here: controller-gen collects RBAC markers
// at package level, and one written into a function's own doc comment is not
// picked up. That is silent — the build succeeds and the rule is simply absent.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=clusters,verbs=get;list;watch

// clusterRelayResolver reads the upstream relay endpoint off the Cluster claim.
//
// The claim is the single place this is written: the Postfix chart's relayHost
// comes from the same field, so a validator reading anywhere else could pass
// against a server the cluster does not actually send through.
//
// Unstructured, to avoid importing the Crossplane API surface for two strings.
func clusterRelayResolver(mgr manager.Manager) RelayResolver {
	return func(ctx context.Context) (string, string, error) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "gentianos.io", Version: "v1alpha1", Kind: "ClusterList",
		})
		if err := mgr.GetClient().List(ctx, list); err != nil {
			return "", "", fmt.Errorf("reading the Cluster claim: %w", err)
		}
		if len(list.Items) == 0 {
			return "", "", fmt.Errorf("this cluster has no Cluster claim to read the relay from")
		}
		spec, _, _ := unstructured.NestedMap(list.Items[0].Object, "spec", "mail")
		host, _ := spec["host"].(string)
		// port is an integer on the claim and a string everywhere it is used.
		var port string
		switch p := spec["port"].(type) {
		case int64:
			port = strconv.FormatInt(p, 10)
		case float64:
			port = strconv.FormatInt(int64(p), 10)
		case string:
			port = p
		}
		return host, port, nil
	}
}

// loadBaoCA reads the CA that signs OpenBao's serving certificate.
//
// Read from the API rather than mounted, because a Pod can only mount Secrets
// from its own namespace and this one lives in OpenBao's. That is the same
// reason ESO's ClusterSecretStore uses a caProvider instead of a volume, and
// this reads the same Secret and the same key.
//
// Read once, at startup, through the API reader rather than the manager's
// cache — the cache is not running yet, and caching every Secret in the
// cluster to fetch one certificate is not a trade worth making.
//
// Returns nil when it cannot be found, which keeps the system roots: a cluster
// that gave OpenBao a publicly trusted certificate needs no CA here, and
// failing startup over a missing one would break it.
func loadBaoCA(mgr manager.Manager) []byte {
	log := ctrl.Log.WithName("custodian")
	if path := os.Getenv("BAO_CACERT"); path != "" {
		pem, err := os.ReadFile(path)
		if err != nil {
			log.Error(err, "reading BAO_CACERT", "path", path)
			return nil
		}
		return pem
	}

	name := envOr("BAO_CA_SECRET", "openbao-tls")
	namespace := envOr("BAO_CA_SECRET_NAMESPACE", "openbao")
	sec := &corev1.Secret{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, sec); err != nil {
		// An in-cluster address over https is a certificate no public root
		// signs, so this is not the conditional warning it used to be: the
		// custodian's login WILL fail, and saying it at Info under an "if" is
		// how it scrolled past on a cluster where the Secret was simply
		// being looked for in the wrong namespace.
		if addr := os.Getenv("BAO_ADDR"); strings.HasPrefix(addr, "https://") &&
			strings.Contains(addr, ".svc") {
			log.Error(err, "OpenBao's CA was not found, so the custodian cannot log in to it: "+
				"nothing in the cluster can verify an in-cluster certificate against the public roots. "+
				"Set custodian.caSecretNamespace to the namespace the vault runs in "+
				"(it follows openbaoNamespace by default), or BAO_CACERT to a file.",
				"secret", namespace+"/"+name, "address", addr)
			return nil
		}
		log.Info("no OpenBao CA available; verifying against the system roots instead. "+
			"That is correct for a vault with a publicly trusted certificate, and fatal to "+
			"the custodian's login for one without.",
			"secret", namespace+"/"+name, "reason", err.Error())
		return nil
	}
	// ca.crt first: on a cert-manager-issued Secret it is the issuer, while
	// tls.crt is the leaf. A leaf works only while it is the one being served,
	// so trusting the issuer survives renewal.
	for _, key := range []string{"ca.crt", "tls.crt"} {
		if pem, ok := sec.Data[key]; ok && len(pem) > 0 {
			log.Info("trusting OpenBao's CA", "secret", namespace+"/"+name, "key", key)
			return pem
		}
	}
	log.Info("OpenBao CA Secret has neither ca.crt nor tls.crt", "secret", namespace+"/"+name)
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// caller carries the authenticated identity for one request.
type caller struct {
	// name is the human recorded as having set a credential.
	name string
	// view is what this caller may see and set: the authorization store's
	// answer, never the request's.
	view Viewer
}

// bearer pulls the OIDC token out of the request. It performs no authorisation:
// that is the store's, asked in identify.
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

// identify establishes who the caller is and what they may do.
//
// Scope and tenant were once read from a query parameter and a header, which
// made them claims the caller made about itself. Then the caller's token was
// exchanged at the vault and the vault's verdict was taken: a policy meant
// cluster administrator, a mapped claim meant a tenant. That was verified,
// but it made a group written into a token a second source of rights.
//
// Now the token is verified here and the authorization store is asked, as at
// every other enforcement point, and that is the whole of it. The token is
// not passed on to the vault or to anything else, is never logged and never
// stored: it lives for the duration of one request.
func (s *Server) identify(ctx context.Context, r *http.Request) (caller, error) {
	tok, err := bearer(r)
	if err != nil {
		return caller{}, err
	}
	if s.Authz == nil {
		return caller{}, fmt.Errorf("%w: none is configured", ErrAuthorizationUnavailable)
	}
	who, err := s.Authz.Identify(ctx, tok)
	if err != nil {
		return caller{}, err
	}
	view, err := s.viewOf(ctx, who)
	if err != nil {
		return caller{}, err
	}
	c := caller{view: view, name: who.Name}
	s.recordHandover(ctx, c)
	return c, nil
}

// recordHandover notes that the human write path works, best effort.
//
// The installer's bootstrap token is only given up once something else has
// been seen to be able to write to the vault. That something is this: a
// cluster administrator, verified and allowed by the store, reached the
// custodian, and the custodian logged in to the vault as itself. Every other
// check in the installer establishes that the parts are present; this one
// establishes that they open.
//
// Never fails the request. A caller who has just been authorised should not be
// refused because a ConfigMap write lost a conflict — and the next request
// records it anyway.
func (s *Server) recordHandover(ctx context.Context, c caller) {
	if s.Client == nil || s.HandoverNamespace == "" {
		return
	}
	// Cluster admin only. A tenant admin setting a credential shows their own
	// scope opens, which is worth having but is not what the bootstrap token
	// is being traded for.
	if !c.view.ClusterAdmin {
		return
	}
	// The vault half of the proof. A custodian that cannot log in has not
	// shown that anybody can write, however entitled the caller is.
	if _, err := s.Bao.Token(ctx); err != nil {
		ctrl.Log.WithName("custodian").Error(err, "the write path is not proven: the custodian cannot log in to OpenBao")
		return
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if err := handover.RecordWritePathProven(ctx, s.Client, s.HandoverNamespace, c.name, now()); err != nil {
		ctrl.Log.WithName("custodian").Error(err, "recording the handover proof")
	}
}

// writeIdentityErr maps an identify() failure onto a status an operator can act on.
//
// A caller who could not be authorised and an OpenBao that could not be reached
// are different faults with different owners, and collapsing them into 401 cost
// a real afternoon: the portal renders 401 as "OpenBao refused the token —
// check that you are in the cluster-admin group", which was sound advice about
// entirely the wrong thing while the actual fault was a TLS trust gap that
// produced no log line anywhere.
//
// So an upstream failure is 502 and is logged. The log is the point: the
// caller gets a deliberately vague message either way, and without a log an
// operator has nothing at all to work from.
func (s *Server) writeIdentityErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrAuthorizationUnavailable) {
		// Nothing is assumed in the store's place, and the caller is told it
		// is not about them.
		ctrl.Log.WithName("custodian").Error(err, "cannot ask the authorization store about this request")
		writeErr(w, http.StatusServiceUnavailable,
			fmt.Errorf("the custodian cannot ask the authorization store; this is not a problem with your account"))
		return
	}
	writeErr(w, http.StatusUnauthorized, err)
}

// writeVaultErr answers a failure of the custodian's own access to the vault.
// It is never about the caller, who has been authorised by the time it is
// reached, and says so.
func (s *Server) writeVaultErr(w http.ResponseWriter, err error) {
	ctrl.Log.WithName("custodian").Error(err, "the custodian cannot use OpenBao")
	writeErr(w, http.StatusBadGateway,
		fmt.Errorf("the custodian cannot reach or log in to OpenBao; this is not a problem with your account"))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		s.writeIdentityErr(w, err)
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
		s.writeIdentityErr(w, err)
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
	token, err := s.Bao.Token(ctx)
	if err != nil {
		ctrl.Log.WithName("custodian").Error(err, "credentials are listed without who set them")
		return
	}
	for i := range items {
		md, err := s.Bao.Metadata(ctx, token, items[i].VaultPath)
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
		s.writeIdentityErr(w, err)
		return
	}
	name := r.PathValue("name")
	req, err := s.Catalogue.Get(r.Context(), name, c.view)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}

	// Seeing a credential is not being allowed to set it: an auditor reads the
	// list and writes nothing.
	if !c.view.canWrite(req.Scope, req.Tenant) {
		writeErr(w, http.StatusForbidden,
			fmt.Errorf("you may see %s and may not set it", name))
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
	unvalidated := ""
	if req.Validator != "" && req.Validator != "noop" {
		if err := s.Validator.Validate(r.Context(), req.Validator, req.ValidateHost, body.Fields); err != nil {
			// Nothing to probe is a gap in the requirement, not a fact about
			// the credential. Refusing the write there is the wrong way
			// round: the credential is still needed and the declaration is
			// what is incomplete, so it is stored and reported as
			// unvalidated. The deployments token could not be set at all
			// until this distinction existed -- its entry asks for a
			// git-https probe and names no host.
			if errors.Is(err, ErrNoEndpoint) {
				unvalidated = err.Error()
				ctrl.Log.WithName("custodian").Info(
					"storing a credential that could not be validated",
					"requirement", req.Name, "validator", req.Validator, "reason", unvalidated)
			} else {
				writeErr(w, http.StatusUnprocessableEntity,
					fmt.Errorf("validation failed against the target endpoint: %w", err))
				return
			}
		}
	}

	// The caller's token performs the write. If the caller's policy forbids the
	// path, nothing is stored — the service has no authority to fall back on.
	//
	// Logged either way, and not all one status. Every failure here used to be
	// 403, which reads as "your policy forbids this" — so a write that failed
	// because OpenBao was unreachable, or because the mount rejected the
	// payload, presented as a permissions problem and sent an operator to audit
	// a policy that was already correct. The same collapse cost an afternoon
	// one layer up, in identify.
	token, err := s.Bao.Token(r.Context())
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}
	if err := s.Bao.Write(r.Context(), token, req.VaultPath, body.Fields, c.name); err != nil {
		log := ctrl.Log.WithName("custodian")
		if errors.Is(err, ErrUpstream) {
			log.Error(err, "cannot reach OpenBao to store this credential", "path", req.VaultPath)
			writeErr(w, http.StatusBadGateway,
				fmt.Errorf("the custodian cannot reach OpenBao; the credential was not stored"))
			return
		}
		log.Error(err, "OpenBao refused the write", "path", req.VaultPath, "setBy", c.name)
		writeErr(w, http.StatusForbidden, err)
		return
	}

	// Stored is not the same as in use. Everything that reads this path is told
	// to read it again now, rather than at the end of a refresh interval it
	// cannot see. See refreshConsumers for the failure that motivates it.
	s.refreshConsumers(r.Context(), req.VaultPath)

	// Metadata only in the response, as everywhere else.
	out := map[string]any{
		"name":      name,
		"vaultPath": req.VaultPath,
		"stored":    true,
		"setBy":     c.name,
	}
	// Said out loud rather than implied by its absence. A caller who asked for
	// a validated write and got an unvalidated one should be told which they
	// got, on the screen, at the time -- not left to infer it from a status
	// field they were not looking at.
	if unvalidated != "" {
		out["validated"] = false
		out["validationSkipped"] = unvalidated
	} else if req.Validator != "" && req.Validator != "noop" {
		out["validated"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// checkFields enforces the declared schema before anything is sent anywhere.
//
// Every violation is collected rather than returned on the first one found,
// so a form can flag every offending field from a single submission instead
// of discovering the second problem only after resubmitting.
func checkFields(req *Status, got map[string]string) error {
	if len(got) == 0 {
		return fmt.Errorf("no fields supplied")
	}
	declared := map[string]Field{}
	for _, f := range req.Fields {
		declared[f.Key] = f
	}
	var unknown []string
	for k := range got {
		if _, ok := declared[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	var errs FieldErrors
	for _, k := range unknown {
		errs = append(errs, FieldError{Field: k, Message: fmt.Sprintf("unknown field for requirement %q", req.Name)})
	}
	for _, f := range req.Fields {
		v, present := got[f.Key]
		switch {
		case !present:
			errs = append(errs, FieldError{Field: f.Key, Message: "required"})
		case strings.TrimSpace(v) != v:
			// The single most common way a pasted credential breaks.
			errs = append(errs, FieldError{Field: f.Key, Message: "has leading or trailing whitespace"})
		case f.MinLength > 0 && len(v) < f.MinLength:
			errs = append(errs, FieldError{Field: f.Key, Message: fmt.Sprintf("must be at least %d characters", f.MinLength)})
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		ctrl.Log.WithName("custodian").Error(err, "encoding response")
	}
}

// writeErr's response carries an "error" string in every case, so an existing
// caller reading only that field keeps working. "fields" is additive: present
// only when the failure attributes to specific fields, which errors.As finds
// through any %w wrapping — handleSet wraps the Validator's FieldErrors in a
// summary message, and this still unwraps to the same slice.
func writeErr(w http.ResponseWriter, code int, err error) {
	body := map[string]any{"error": err.Error()}
	var fe FieldErrors
	if errors.As(err, &fe) {
		body["fields"] = []FieldError(fe)
	}
	writeJSON(w, code, body)
}
