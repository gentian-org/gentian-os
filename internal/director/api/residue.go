/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
	"github.com/gentian-org/gentian-os/internal/tenancy"
)

// Removing what the catalogue left behind.
//
// Nothing a bundle brought leaves a cluster by itself (gitops/catalogue.go).
// The operator lists what no bundle owns any more and the profiles nobody
// uses, and the usher serves that list. This is the one way to remove an
// entry of it: one object per request, named, with the name typed again.
//
// Two different things happen behind the one route.
//
// A companion -- a Composition, a pack catalog, a ConfigMap, a customization
// record -- is not declared anywhere any more, so removing it is a command:
// the operator works its list out again and deletes the object only if it
// is on it. Before that the director looks in git, at the bundle files the
// catalogue directory holds: an object one of them declares is not asked
// for at all, whatever the cluster says.
//
// An unused profile is declared, in the catalogue directory, so removing it
// is first a commit: its files and its kustomization entries, refused while
// any tenant's manifest names it and unless the operator lists it as unused
// (which is where "no tenant retains data for it" is known). The object is
// still in the cluster then, because the Application that applies the
// directory does not prune. It is deleted by the operator, which does so
// only once Argo CD reports the directory no longer declares it -- so the
// director asks at once, and keeps asking for a while if the answer is "not
// yet". A director that restarts in between forgets to; the profile is then
// still on the list, and asking again finishes it.

//
// A tenant's administrator has a narrower way in, for the leftovers of an
// app the tenant has (removeAppResidue). It is the same command to the
// operator with the tenant and the app named in it, and exists only on a
// cluster whose one user tenant is theirs.

const (
	residuePath       = "/v1/catalogue/residue"
	removeResiduePath = "/v1/actions/remove-catalogue-residue"
	// refusedStillDeclared is the operator's refusal that ends by itself:
	// Argo CD has not taken the removal in yet (applifecycle).
	refusedStillDeclared = "still-declared"
	// refusedShared is the refusal of a tenant's removal on a cluster where
	// the pieces are every tenant's.
	refusedShared      = "shared"
	classUnusedProfile = "unused-profile"
)

// residueWait is for how long the operator is asked again to delete a
// profile Argo CD still found declared; residuePoll is how often, in
// nanoseconds. The second is read by a goroutine that outlives the request
// that started it, and a test shortens it.
const residueWait = 15 * time.Minute

var residuePoll atomic.Int64

func init() { residuePoll.Store(int64(10 * time.Second)) }

// objectName is a Kubernetes object's name: what a companion's is, dots
// included.
var objectName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// residueKinds are the kinds the route removes; true for a companion.
var residueKinds = map[string]bool{
	profilebundle.KindComposition:   true,
	profilebundle.KindOIDCPacks:     true,
	profilebundle.KindConfigMap:     true,
	profilebundle.KindCustomization: true,
	profilebundle.KindProfile:       false,
}

type residueTarget struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// appScope is the tenant and the app a tenant's removal is asked for. The
// operator removes for it only a leftover of that app, which the tenant has.
type appScope struct {
	Tenant  string `json:"tenant"`
	Profile string `json:"profile"`
}

// operatorRemoval is what the operator is asked: one object, and for a
// tenant's removal on whose behalf.
type operatorRemoval struct {
	residueTarget
	*appScope
}

// operatorAnswer is what the operator says to a removal.
type operatorAnswer struct {
	Status  string          `json:"status"`
	Deleted json.RawMessage `json:"deleted"`
	Message string          `json:"message"`
	// On a refusal.
	Detail string `json:"detail"`
	Reason string `json:"reason"`
}

func (s *Server) removeCatalogueResidue(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		residueTarget
		Confirm string `json:"confirm"`
	}
	usage := `body must be {"kind": "Composition|OIDCPackCatalog|ConfigMap|Customization|ComponentProfile", ` +
		`"name": "<name>", "namespace": "<optional>", "confirm": "<the name again>"}`
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, usage)
		return
	}
	companion, known := residueKinds[body.Kind]
	if !known || !objectName.MatchString(body.Name) || (body.Namespace != "" && !gitops.ValidName(body.Namespace)) {
		s.fail(w, r, http.StatusBadRequest, usage)
		return
	}
	target := body.residueTarget
	// The name typed again, before anything is looked at: this deletes.
	if body.Confirm != body.Name {
		s.json(w, http.StatusPreconditionRequired, map[string]any{
			"error": fmt.Sprintf("removing the %s %s deletes it from the cluster, and nothing puts it back "+
				"but installing a build that brings it", body.Kind, body.Name),
			"confirmField":   "confirm",
			"confirmWith":    body.Name,
			"dangerous":      true,
			"requiresRetype": true,
			"request_id":     reqID(r.Context()),
		})
		return
	}
	if companion {
		s.removeCompanion(w, r, c, target, nil)
		return
	}
	s.removeUnusedProfile(w, r, c, target)
}

// removeAppResidue removes one leftover of an app for the administrator of
// a tenant that has it.
//
// What an app's bundle brought is on the cluster once, for every tenant that
// installed the app, so the relation on the tenant is not the whole check.
// It is asked first -- can_install_app, as uninstalling and purging the app
// are -- and then the cluster's tenancy mode is read from git: only where it
// is single, and this is the one user tenant such a cluster carries, are the
// pieces this tenant's alone. Everywhere else the answer is 403 with the
// sentence the console shows, before the name is asked for again: nobody is
// made to type a name to be refused.
//
// Then it is the cluster's removal of a companion, with the tenant and the
// app passed along. The operator works its list out again and deletes the
// object only if it is on it as something a newer build of this app left
// behind, and only if the tenant has the app; it refuses under its own
// tenancy mode too. An object no bundle owns and a profile nobody uses are
// not reachable from here.
func (s *Server) removeAppResidue(w http.ResponseWriter, r *http.Request, c call) {
	scope := &appScope{Tenant: r.PathValue("t"), Profile: r.PathValue("p")}
	if !gitops.ValidName(scope.Profile) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return
	}
	settings, err := s.cfg.Repo.ClusterSettingValues(r.Context())
	if err != nil && !errors.Is(err, gitops.ErrNoClusterClaim) {
		s.repoError(w, r, err)
		return
	}
	// No claim is no word that the cluster is single, and so it is not.
	if !tenancy.SoleUserTenant(settings["tenancyMode"], scope.Tenant) {
		s.json(w, http.StatusForbidden, map[string]any{
			"error": tenancy.SharedPieces, "reason": refusedShared, "removableBy": "platform",
			"request_id": reqID(r.Context()),
		})
		return
	}
	var body struct {
		residueTarget
		Confirm string `json:"confirm"`
	}
	usage := `body must be {"kind": "Composition|OIDCPackCatalog|ConfigMap|Customization", ` +
		`"name": "<name>", "namespace": "<optional>", "confirm": "<the name again>"}`
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, usage)
		return
	}
	// A companion and nothing else: a profile is the cluster's to retire.
	if !residueKinds[body.Kind] || !objectName.MatchString(body.Name) ||
		(body.Namespace != "" && !gitops.ValidName(body.Namespace)) {
		s.fail(w, r, http.StatusBadRequest, usage)
		return
	}
	if body.Confirm != body.Name {
		s.json(w, http.StatusPreconditionRequired, map[string]any{
			"error": fmt.Sprintf("removing the %s %s deletes it from the cluster, and nothing puts it back "+
				"but installing a build that brings it; a build of %s older than the one installed that needed it "+
				"would no longer find it", body.Kind, body.Name, scope.Profile),
			"confirmField":   "confirm",
			"confirmWith":    body.Name,
			"dangerous":      true,
			"requiresRetype": true,
			"request_id":     reqID(r.Context()),
		})
		return
	}
	s.removeCompanion(w, r, c, body.residueTarget, scope)
}

// removeCompanion has the operator delete one object no bundle owns: for the
// cluster when scope is nil, and otherwise for the tenant and the app named.
func (s *Server) removeCompanion(w http.ResponseWriter, r *http.Request, c call, target residueTarget, scope *appScope) {
	// What git says first. A bundle file in the catalogue directory that
	// declares the object owns it, whether or not the cluster has caught up.
	owner, err := s.cfg.Repo.CatalogueDeclares(r.Context(), target.Kind, target.Name)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if owner != "" && scope != nil && owner != scope.Profile {
		// Whose it is, is not a tenant's to be told.
		s.refuseResidue(w, r, "still-declared", fmt.Sprintf(
			"the %s %s is declared by a bundle in the deployments repository, so it is not residue. Nothing was deleted",
			target.Kind, target.Name))
		return
	}
	if owner != "" {
		s.refuseResidue(w, r, "still-declared", fmt.Sprintf(
			"the %s %s is declared by the bundle of profile %s in the deployments repository, so it is not residue. Nothing was deleted",
			target.Kind, target.Name, owner))
		return
	}
	status, answer, err := s.askRemoval(r.Context(), c.meta.ActorName(), operatorRemoval{target, scope})
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	if status < 200 || status > 299 {
		s.relayRefusal(w, r, status, answer)
		return
	}
	s.json(w, http.StatusAccepted, map[string]any{
		"status": answer.Status, "deleted": answer.Deleted, "message": answer.Message,
	})
}

// removeUnusedProfile takes a profile nobody uses out of git, and has the
// operator delete it from the cluster once that has been taken in.
func (s *Server) removeUnusedProfile(w http.ResponseWriter, r *http.Request, c call, target residueTarget) {
	ctx, name := r.Context(), target.Name
	if !gitops.ValidName(name) || target.Namespace != "" {
		s.fail(w, r, http.StatusBadRequest, "a ComponentProfile has a plain name and no namespace")
		return
	}
	// The cluster's word that it is unused: nobody has it installed there,
	// and no tenant retains data for it. Only the operator knows the second.
	if why, err := s.listedAsUnused(ctx, name); err != nil {
		s.lifecycleError(w, r, err)
		return
	} else if why != "" {
		s.refuseResidue(w, r, "not-residue", why)
		return
	}
	// And git's, inside the commit that removes it.
	res, err := s.cfg.Repo.RetireProfile(ctx, name, c.meta)
	var inUse *gitops.ErrProfileInUse
	if errors.As(err, &inUse) {
		s.refuseResidue(w, r, "not-residue", fmt.Sprintf(
			"the ComponentProfile %s is not removed: %s. Nothing was changed", name, inUse.Why))
		return
	}
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	out := map[string]any{"deleted": nil}
	committed := ""
	if res.Changed {
		out["commit"] = res.Commit
		committed = "Its files were removed from the deployments repository. "
	}

	status, answer, err := s.askRemoval(ctx, c.meta.ActorName(), operatorRemoval{residueTarget: target})
	switch {
	case err == nil && status >= 200 && status <= 299:
		out["status"], out["deleted"], out["message"] = answer.Status, answer.Deleted, committed+answer.Message
	case err == nil && status == http.StatusConflict && answer.Reason != refusedStillDeclared && !res.Changed:
		// Nothing was committed and the cluster will not have it deleted.
		s.relayRefusal(w, r, status, answer)
		return
	default:
		// Not yet: Argo CD has to take the commit in first, or the operator
		// could not be asked just now. Asked again from here, for a while.
		why := "the operator could not be asked"
		if err == nil {
			why = answer.Detail
		}
		s.finishRetirementLater(name, c.meta.ActorName(), target)
		out["status"] = "pending"
		if res.Changed {
			out["status"] = "committed"
		}
		out["message"] = fmt.Sprintf("%sThe ComponentProfile %s is still on the cluster: %s. It is asked for again "+
			"until it is deleted, for up to %s; the residue list shows it until then, and this request can be repeated.",
			committed, name, why, residueWait)
	}
	s.json(w, http.StatusAccepted, out)
}

// listedAsUnused answers "" when the operator lists the profile as unused,
// and otherwise why it is not removed.
func (s *Server) listedAsUnused(ctx context.Context, name string) (string, error) {
	status, body, err := s.cfg.Lifecycle.Get(ctx, residuePath, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", &lifecycle.UpstreamError{Status: status, Message: lifecycle.ErrorMessage(body)}
	}
	var list struct {
		Residue []struct {
			Kind  string `json:"kind"`
			Name  string `json:"name"`
			Class string `json:"class"`
		} `json:"residue"`
		Incomplete []string `json:"incomplete"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("app-lifecycle API: residue: %w", err)
	}
	for _, item := range list.Residue {
		if item.Kind == profilebundle.KindProfile && item.Name == name && item.Class == classUnusedProfile {
			return "", nil
		}
	}
	why := fmt.Sprintf("the cluster does not list the ComponentProfile %s as an unused profile: a tenant has it "+
		"installed or retains data for it, it is not one a catalogue brought, or it is not there", name)
	for _, note := range list.Incomplete {
		why += "; " + note
	}
	return why + ". Nothing was changed", nil
}

// askRemoval puts one removal to the operator.
func (s *Server) askRemoval(ctx context.Context, actor string, target operatorRemoval) (int, operatorAnswer, error) {
	status, body, err := s.cfg.Lifecycle.Do(ctx, removeResiduePath, actor, target)
	if err != nil {
		return 0, operatorAnswer{}, err
	}
	var answer operatorAnswer
	if json.Unmarshal(body, &answer) != nil || (answer.Detail == "" && answer.Status == "") {
		answer.Detail = lifecycle.ErrorMessage(body)
	}
	return status, answer, nil
}

// relayRefusal passes the operator's refusal on, with which refusal it was.
func (s *Server) relayRefusal(w http.ResponseWriter, r *http.Request, status int, answer operatorAnswer) {
	if status == http.StatusForbidden && answer.Reason == refusedShared {
		// The operator runs under a tenancy mode that is not the one git
		// declares. Its word is the cluster's, and it deleted nothing.
		s.json(w, http.StatusForbidden, map[string]any{
			"error": answer.Detail, "reason": refusedShared, "removableBy": "platform", "request_id": reqID(r.Context()),
		})
		return
	}
	if status != http.StatusConflict {
		s.fail(w, r, status, answer.Detail)
		return
	}
	s.refuseResidue(w, r, answer.Reason, answer.Detail)
}

// refuseResidue answers a removal that deleted nothing: 409, why, and which
// kind of refusal -- "not-residue", "still-declared" or "changed".
func (s *Server) refuseResidue(w http.ResponseWriter, r *http.Request, reason, why string) {
	s.json(w, http.StatusConflict, map[string]any{"error": why, "reason": reason, "request_id": reqID(r.Context())})
}

// finishRetirementLater keeps asking the operator to delete a profile that
// has left git, once per profile, until it is deleted, refused for good, or
// residueWait is over.
func (s *Server) finishRetirementLater(name, actor string, target residueTarget) {
	if _, busy := s.retiring.LoadOrStore(name, struct{}{}); busy {
		return
	}
	go func() {
		defer s.retiring.Delete(name)
		ctx, cancel := context.WithTimeout(context.Background(), residueWait)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				s.cfg.Log.Warn("an unused profile left git and is still on the cluster; it stays on the residue list",
					"profile", name)
				return
			case <-time.After(time.Duration(residuePoll.Load())):
			}
			status, answer, err := s.askRemoval(ctx, actor, operatorRemoval{residueTarget: target})
			switch {
			case err != nil:
				s.cfg.Log.Warn("asking the operator to delete an unused profile", "profile", name, "error", err.Error())
			case status >= 200 && status <= 299:
				s.cfg.Log.Info("an unused profile was deleted from the cluster", "profile", name, "status", answer.Status)
				return
			case status == http.StatusConflict && answer.Reason == refusedStillDeclared:
			default:
				s.cfg.Log.Warn("the operator refused to delete an unused profile; it stays on the cluster",
					"profile", name, "status", status, "detail", answer.Detail)
				return
			}
		}
	}()
}
