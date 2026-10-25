/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openfga/language/pkg/go/transformer"
)

// Compile turns a profile's model, written in the OpenFGA DSL, into the JSON
// OpenFGA's API takes. The operator compiles a model when the profile is
// admitted and refuses one that does not compile, so a broken model is a
// failed install and never a store without a model; the arbiter only ever
// sees what came out of here.
//
// The DSL is compiled, not checked further: whether every relation a
// definition names exists is OpenFGA's to say when the model is written,
// and saying it twice is two answers that can disagree.
func Compile(dsl string) (json.RawMessage, error) {
	model, err := transformer.TransformDSLToProto(dsl)
	if err != nil {
		return nil, fmt.Errorf("the authorization model does not compile: %w", err)
	}
	if len(model.GetTypeDefinitions()) == 0 {
		return nil, errors.New("the authorization model defines no type")
	}
	out, err := transformer.TransformDSLToJSON(dsl)
	if err != nil {
		return nil, fmt.Errorf("the authorization model does not compile: %w", err)
	}
	// Re-encoded, so that one model is always the same bytes. protojson
	// varies its whitespace from one build of it to the next on purpose, and
	// a digest of its output would call the same model new on every
	// operator upgrade. encoding/json sorts the keys and adds nothing.
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("the compiled authorization model: %w", err)
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("the compiled authorization model: %w", err)
	}
	return json.RawMessage(canonical), nil
}

// ModelDigest is what tells one compiled model from another: the SHA-256 of
// its JSON. The arbiter writes a new model to a store when the digest of the
// model it was handed is not the one it wrote last.
func ModelDigest(compiled json.RawMessage) string {
	sum := sha256.Sum256(compiled)
	return hex.EncodeToString(sum[:])
}
