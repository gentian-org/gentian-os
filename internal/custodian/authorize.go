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

package custodian

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianv1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// Who may touch a credential is the authorization store's to say, and only
// the store's.
//
// The custodian used to take OpenBao's verdict on the caller's token as the
// answer: a policy on the exchanged token meant "cluster administrator", a
// claim mapped into its metadata meant "this tenant". That made OpenBao a
// second place rights were decided, from a group written into a token, beside
// the store every other enforcement point asks. Two authorities disagree
// sooner or later, and the one nobody reviews is the one that is wrong.
//
// So the custodian verifies the caller's token itself, as the director, the
// bouncer and the usher do, and asks the store one of two questions about the
// scope a credential belongs to -- the cluster, or one tenant:
//
//	can_read_credential    see that it is required, whether it is set, by whom
//	can_write_credential   set it
//
// Reading is never reading a value: no route returns one.
//
// OpenBao still receives the caller's own token for the write, and its policy
// still bounds which paths that token reaches. That is a second lock on the
// same door. It can refuse what the store allowed, loudly; it is never asked
// first and nothing it says widens what the store said.
const (
	relationRead  = "can_read_credential"
	relationWrite = "can_write_credential"

	scopeCluster = "cluster"
	scopeTenant  = "tenant"
)

// ErrUnauthenticated is a token that could not be verified.
var ErrUnauthenticated = errors.New("the token could not be verified")

// ErrAuthorizationUnavailable is the store not answering. Nothing is assumed
// in its place: a custodian that cannot ask allows nothing.
var ErrAuthorizationUnavailable = errors.New("the authorization store did not answer")

// Principal is a verified caller.
type Principal struct {
	// User is the caller as the store names them: user:<subject>.
	User string
	// Name is for the record of who set a credential. Never used to decide.
	Name string
	// Tenant is the tenant whose realm signed the caller in, or empty for the
	// kernel realm, whose people are the cluster's. It says where a caller
	// stands, not what they may do there: that is still asked.
	Tenant string
}

// Authorizer is how the custodian learns who is calling and what they may do.
type Authorizer interface {
	// Identify verifies a token and says who presented it.
	Identify(ctx context.Context, token string) (Principal, error)
	// Check asks the store whether user holds relation on object.
	Check(ctx context.Context, user, relation, object string) (bool, error)
	// Object names a credential scope as the store does: the cluster, or a
	// tenant.
	Object(scope, tenant string) (string, bool)
}

// GraphAuthorizer is the Authorizer over the cluster's identity provider and
// its OpenFGA store.
type GraphAuthorizer struct {
	Verifier *authn.Verifier
	Graph    authz.Checker
	// Cluster is this cluster's id, the object kernel and system credentials
	// are asked about.
	Cluster string
	// KernelRealm is the realm the cluster's own administrators sign in to.
	KernelRealm string
	// Client reads the Tenants, to tell which tenant a realm belongs to.
	Client client.Reader
}

// Identify verifies the token against the realm that issued it.
func (a *GraphAuthorizer) Identify(ctx context.Context, token string) (Principal, error) {
	ident, err := a.Verifier.Verify(ctx, token)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	user, err := authz.User(ident.Subject)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	p := Principal{User: user, Name: ident.Email}
	if p.Name == "" {
		p.Name = ident.Name
	}
	if ident.Realm == "" || ident.Realm == a.KernelRealm {
		return p, nil
	}
	tenant, err := a.tenantOfRealm(ctx, ident.Realm)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrAuthorizationUnavailable, err)
	}
	p.Tenant = tenant
	return p, nil
}

// tenantOfRealm is the tenant a realm belongs to: the one that names it, or
// the one of the same name, which is what a tenant's realm is called unless
// it says otherwise. A realm no tenant has yields no tenant, and a caller
// from it stands nowhere.
func (a *GraphAuthorizer) tenantOfRealm(ctx context.Context, realm string) (string, error) {
	var tenants gentianv1alpha1.TenantList
	if err := a.Client.List(ctx, &tenants); err != nil {
		return "", fmt.Errorf("listing tenants: %w", err)
	}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		named := t.Name
		if t.Spec.Isolation != nil && t.Spec.Isolation.KeycloakRealm != "" {
			named = t.Spec.Isolation.KeycloakRealm
		}
		if named == realm {
			return t.Name, nil
		}
	}
	return "", nil
}

// Check asks the store.
func (a *GraphAuthorizer) Check(ctx context.Context, user, relation, object string) (bool, error) {
	return a.Graph.Check(ctx, "", user, relation, object)
}

// Object is the store's name for a scope.
func (a *GraphAuthorizer) Object(scope, tenant string) (string, bool) {
	switch scope {
	case scopeCluster:
		return authz.Cluster(a.Cluster), a.Cluster != ""
	case scopeTenant:
		return authz.Tenant(tenant), tenant != ""
	}
	return "", false
}

// viewOf is what one caller may see and set, as the store answers it.
//
// The two things every handler asks about first are settled here, so that a
// store that does not answer fails the request rather than narrowing it: an
// administrator shown an empty list because a lookup failed would conclude
// that nothing is required. Everything else is asked when it is needed and
// remembered for the request.
func (s *Server) viewOf(ctx context.Context, p Principal) (Viewer, error) {
	asked := map[string]bool{}
	var failed error
	check := func(relation, scope, tenant string) bool {
		object, ok := s.Authz.Object(scope, tenant)
		if !ok {
			return false
		}
		key := relation + " " + object
		if answer, known := asked[key]; known {
			return answer
		}
		allowed, err := s.Authz.Check(ctx, p.User, relation, object)
		if err != nil {
			if failed == nil {
				failed = err
			}
			return false
		}
		asked[key] = allowed
		return allowed
	}
	v := Viewer{check: check}
	v.ClusterAdmin = check(relationWrite, scopeCluster, "")
	// Where the caller stands counts only if they may act there. A member of
	// a tenant who administers nothing has no tenant as far as credentials
	// go, which is what closes every handler that acts "for the caller's
	// tenant" to them.
	if p.Tenant != "" && check(relationWrite, scopeTenant, p.Tenant) {
		v.Tenant = p.Tenant
	}
	if failed != nil {
		return Viewer{}, fmt.Errorf("%w: %v", ErrAuthorizationUnavailable, failed)
	}
	return v, nil
}
