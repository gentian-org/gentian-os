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
	"net/url"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// What became of a tenant's apps, and the two things done to one of them that
// are not a change to what the tenant is.
//
// GET /v1/tenants/{t}/apps answers from git: what the tenant is meant to have.
// What the cluster made of it -- still coming up, running, broken -- is the
// operator's to say, so it is relayed rather than recomputed here.
//
// Purging and provisioning are actions. Neither is desired state: a purge
// deletes what an app that is already gone left behind, and provisioning
// puts the people who are members today into a group. There is nothing to
// commit, so the person is named on the request instead of on a commit.

func appsPath(r *http.Request, suffix string) string {
	return "/v1/tenants/" + url.PathEscape(r.PathValue("t")) + suffix
}

func (s *Server) appStates(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, appsPath(r, "/apps/status"))
}

// appAction relays one action about one profile.
func (s *Server) appAction(w http.ResponseWriter, r *http.Request, c call, action string) {
	var body struct {
		Profile string `json:"profile"`
	}
	if err := decode(r, &body); err != nil || !gitops.ValidName(body.Profile) {
		s.fail(w, r, http.StatusBadRequest, `body must be {"profile": "<name>"}`)
		return
	}
	status, answer, err := s.cfg.Lifecycle.Do(r.Context(),
		appsPath(r, "/actions/"+action), c.meta.ActorName(), map[string]string{"profile": body.Profile})
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.started(w, r, status, answer)
}

// purgeApp deletes the data an uninstalled app left behind. The operator
// refuses while the tenant still has the app, so this cannot take data from
// something that is running.
func (s *Server) purgeApp(w http.ResponseWriter, r *http.Request, c call) {
	s.appAction(w, r, c, "purge-app")
}

// provisionApp grants an installed app to everybody who is a member now.
func (s *Server) provisionApp(w http.ResponseWriter, r *http.Request, c call) {
	s.appAction(w, r, c, "provision-app")
}
