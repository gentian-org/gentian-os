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
	"net/http"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// clusterModels answers the model settings of the Cluster claim, and the
// models they make the gateway offer, each with what the claim alone says
// about whether it works.
//
// From git, like every read of this service: the gateway is not asked, and
// neither is the vault. So an instance's model is reported as not served
// because the platform starts no instance, and a provider's model as
// declared, with the name of the credential its token belongs under -- whether
// that credential is there is the custodian's to say.
func (s *Server) clusterModels(w http.ResponseWriter, r *http.Request, _ call) {
	settings, err := s.cfg.Repo.ClusterModels(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{
		"cluster":  s.cfg.Cluster,
		"settings": settings,
		"models":   settings.GatewayModels(),
	})
}

// setClusterModels commits the model settings: the whole of them, so a model
// the body does not name is removed from the claim and with it from the
// gateway.
func (s *Server) setClusterModels(w http.ResponseWriter, r *http.Request, c call) {
	var want gitops.ModelSettings
	if err := decode(r, &want); err != nil {
		s.fail(w, r, http.StatusBadRequest,
			"body must be the model settings: enabled, gpuAcceleration, instances, providers -- and no provider's token, which is a credential")
		return
	}
	res, err := s.cfg.Repo.SetClusterModels(r.Context(), want, c.meta)
	switch {
	case errors.Is(err, gitops.ErrInvalidModels):
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, gitops.ErrModelsFlowForm):
		s.fail(w, r, http.StatusConflict, gitops.ErrModelsFlowForm.Error())
	default:
		s.written(w, r, res, err)
	}
}
