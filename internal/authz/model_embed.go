/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	_ "embed"
	"encoding/json"
)

//go:embed data/model-v0.json
var authorizationModelJSON []byte

// AuthorizationModelPayload returns schema_version + type_definitions for OpenFGA write API.
func AuthorizationModelPayload() (schemaVersion string, typeDefinitions json.RawMessage, err error) {
	var payload struct {
		SchemaVersion   string          `json:"schema_version"`
		TypeDefinitions json.RawMessage `json:"type_definitions"`
	}
	if err := json.Unmarshal(authorizationModelJSON, &payload); err != nil {
		return "", nil, err
	}
	return payload.SchemaVersion, payload.TypeDefinitions, nil
}
