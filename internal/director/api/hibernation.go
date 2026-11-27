/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// A tenant's sleep.
//
// A sleeping tenant runs nothing and keeps everything; the operator does
// both. What is here is the decision: one commit of spec.hibernation, made
// by the cluster's administrator, which the operator reads back from git
// like every other change to a tenant.

// maxAwake bounds how long a tenant is kept awake by one request. A tenant
// that should stay awake for good is woken without a duration.
const maxAwake = 365 * 24 * time.Hour

// hibernateTenant puts a tenant to sleep until somebody wakes it.
func (s *Server) hibernateTenant(w http.ResponseWriter, r *http.Request, c call) {
	s.writeHibernation(w, r, c, gitops.Hibernation{Asleep: true})
}

// wakeTenant wakes a tenant: for good with no body, or with
// {"for": "5d"} until that long from now, after which it sleeps again
// without being asked.
func (s *Server) wakeTenant(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		For string `json:"for"`
	}
	if err := decode(r, &body); err != nil && !errors.Is(err, io.EOF) {
		s.fail(w, r, http.StatusBadRequest, `body must be empty, or {"for": "<duration>"} such as "5d" or "36h"`)
		return
	}
	if strings.TrimSpace(body.For) == "" {
		s.writeHibernation(w, r, c, gitops.Hibernation{})
		return
	}
	awake, err := parseAwake(body.For)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	until := time.Now().Add(awake)
	s.writeHibernation(w, r, c, gitops.Hibernation{Asleep: true, AwakeUntil: &until})
}

func (s *Server) writeHibernation(w http.ResponseWriter, r *http.Request, c call, want gitops.Hibernation) {
	tenant := r.PathValue("t")
	res, err := s.cfg.Repo.SetTenantHibernation(r.Context(), tenant, want, c.meta)
	if errors.Is(err, gitops.ErrPlatformTenantSleeps) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	answer := map[string]any{"status": res.Status, "tenant": tenant, "asleep": want.Asleep && want.AwakeUntil == nil}
	if want.AwakeUntil != nil {
		answer["awakeUntil"] = want.AwakeUntil.UTC().Truncate(time.Second)
	}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}

// parseAwake reads how long a tenant stays awake: whole days ("5d") or
// anything time.ParseDuration reads ("36h", "90m").
func parseAwake(text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	var d time.Duration
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n > int(maxAwake/(24*time.Hour)) {
			return 0, fmt.Errorf("%q is not a duration of at most 365 days: write 5d or 36h", text)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(text); err != nil {
			return 0, fmt.Errorf("%q is not a duration: write 5d or 36h", text)
		}
	}
	if d <= 0 || d > maxAwake {
		return 0, fmt.Errorf("%q is not a duration between a moment and 365 days", text)
	}
	return d, nil
}
