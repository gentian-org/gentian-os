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

package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/security"
)

// Answering a privilege request.
//
// A profile says what it would need beyond the default posture; the component
// holds until somebody answers (AD-5). These handlers are the answer, and they
// are writes in this API's sense: a commit to the deployments repository, with
// the approver taken from the token and never from the body.

// grantPrivilegeRequest is what an approver states. Not who they are, and not
// when: an approval that could name its own approver would record nothing, so
// both come from the call.
type grantPrivilegeRequest struct {
	// Reason in the approver's own words. The profile already said why it
	// wants the privilege; this says why somebody agreed, which is what a
	// reviewer reads later.
	Reason string `json:"reason"`
	// ExpiresAt bounds the grant, RFC 3339. Optional, and for a pod-security
	// waiver it is close to obligatory in practice -- a waiver with no expiry
	// is a waiver nobody reviews.
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// tenantPrivileges answers what has been granted for this tenant, from git.
//
// Under can_view, like every other read of a tenant's declared state: whoever
// may see the tenant may see what was approved for it. Who MAY approve is a
// different question and it is asked on the write.
func (s *Server) tenantPrivileges(w http.ResponseWriter, r *http.Request, _ call) {
	grants, err := s.cfg.Repo.TenantPrivileges(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if grants == nil {
		grants = []gitops.PrivilegeGrant{}
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "privileges": grants})
}

// mayApprove decides whether this caller may answer a request of this kind.
//
// The route itself carries only can_view, because who may approve follows from
// the kind and the kind is not known until the request arrives. This function
// is therefore the authorisation for both writes, and neither handler may skip
// it -- which is why it takes the ResponseWriter and answers the call itself
// rather than returning a reason for a caller to remember to use.
//
//	egress                    the traffic leaves the tenant's own namespace,
//	                          so the tenant's administrator answers:
//	                          tenant#can_approve_privilege.
//	podSecurity, clusterRoles these weaken what protects the NODE, and every
//	                          tenant on it. cluster#can_approve, the security
//	                          officer's verb, and a tenant administrator's
//	                          authority inside their own tenant is not it.
//
// The two are alternatives rather than both. A security officer is
// deliberately not a tenant administrator, and requiring both relations would
// mean nobody could approve a waiver without holding an authority they should
// not need.
// It returns the decision it made, in the form the commit trailer records --
// "can_approve cluster:main" -- because the route's own relation is can_view
// and a trailer saying that would name the wrong authority for the one thing
// an audit of a privilege grant is for.
func (s *Server) mayApprove(w http.ResponseWriter, r *http.Request, c *call, kind string) (string, bool) {
	relation, target, refusal := "can_approve_privilege", "", ""
	if security.PrivilegeScope(kind) == "tenant" {
		tenant, err := tenantObject(r)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid name")
			return "", false
		}
		target = tenant
		refusal = "approving an egress request is the tenant administrator's"
	} else {
		if s.cfg.Cluster == "" {
			// No cluster object to check against, so there is no security
			// officer to be. Refusing is the only safe answer: the
			// alternative is letting a waiver through because the deployment
			// happens not to name its cluster.
			s.fail(w, r, http.StatusForbidden,
				"a "+kind+" privilege is approved at cluster scope, and this director serves no cluster")
			return "", false
		}
		relation, target = "can_approve", authz.Cluster(s.cfg.Cluster)
		refusal = "a " + kind + " privilege weakens what protects the node; approving one is the security officer's"
	}
	ok, err := s.cfg.Authz.Check(r.Context(), reqID(r.Context()), c.user, relation, target)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return "", false
	}
	if !ok {
		s.fail(w, r, http.StatusForbidden, refusal)
		return "", false
	}
	decision := relation + " " + target
	c.meta.Decision = decision
	return decision, true
}

// privilegeRef reads and checks the two path segments that name the request.
// A ref that is not one of the three kinds is a 404 rather than a 400: the
// route exists, the privilege named on it does not.
func (s *Server) privilegeRef(w http.ResponseWriter, r *http.Request) (install, kind, ref string, ok bool) {
	install = r.PathValue("inst")
	kind = r.PathValue("kind")
	name := r.PathValue("name")
	switch kind {
	case security.PrivilegePodSecurity, security.PrivilegeEgress, security.PrivilegeClusterRoles:
	default:
		s.fail(w, r, http.StatusNotFound, "no such privilege kind: "+kind)
		return "", "", "", false
	}
	if install == "" || name == "" {
		s.fail(w, r, http.StatusBadRequest, "the path must name an install and a privilege")
		return "", "", "", false
	}
	return install, kind, security.PrivilegeRef(kind, name), true
}

// grantPrivilege records one approval as a commit.
func (s *Server) grantPrivilege(w http.ResponseWriter, r *http.Request, c call) {
	install, kind, ref, ok := s.privilegeRef(w, r)
	if !ok {
		return
	}
	if _, ok := s.mayApprove(w, r, &c, kind); !ok {
		return
	}
	var body grantPrivilegeRequest
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"reason": "...", "expiresAt": "<RFC 3339>"}`)
		return
	}
	// The minimum the CRD enforces anyway, refused here so the answer says
	// what is wrong rather than surfacing later as a rejected commit.
	if len(strings.TrimSpace(body.Reason)) < 10 {
		s.fail(w, r, http.StatusBadRequest, "a grant needs a reason of at least ten characters, in your own words")
		return
	}
	if body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "expiresAt must be an RFC 3339 timestamp")
			return
		}
		if !t.After(time.Now()) {
			// A grant that has already expired grants nothing, and committing
			// one would read as an approval while the component kept waiting.
			s.fail(w, r, http.StatusBadRequest, "expiresAt is in the past; that grants nothing")
			return
		}
	}
	res, err := s.cfg.Repo.GrantPrivilege(r.Context(), r.PathValue("t"), gitops.PrivilegeGrant{
		Install:    install,
		Privilege:  ref,
		Approver:   c.meta.Subject,
		ApprovedAt: time.Now().UTC().Format(time.RFC3339),
		Reason:     strings.TrimSpace(body.Reason),
		ExpiresAt:  body.ExpiresAt,
	}, c.meta)
	s.written(w, r, res, err)
}

// revokePrivilege withdraws one. The component's next reconcile finds the
// request pending again and holds, which is where it was before the approval:
// a revocation takes something away rather than breaking something.
func (s *Server) revokePrivilege(w http.ResponseWriter, r *http.Request, c call) {
	install, kind, ref, ok := s.privilegeRef(w, r)
	if !ok {
		return
	}
	// Whoever may grant may withdraw, and nobody else. Checking the same
	// requirement on the way out matters as much as on the way in: a tenant
	// administrator who could revoke a pod-security waiver could stop a
	// component the platform operator approved.
	if _, ok := s.mayApprove(w, r, &c, kind); !ok {
		return
	}
	res, err := s.cfg.Repo.RevokePrivilege(r.Context(), r.PathValue("t"), install, ref, c.meta)
	s.written(w, r, res, err)
}
