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
