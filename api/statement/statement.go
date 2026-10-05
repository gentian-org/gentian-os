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

// Package statement is the payload of an entitlement statement: what an App
// Store asserts to a cluster about a tenant and a catalogue entry. A statement
// travels as a compact JWS signed with EdDSA; this is what is inside it, and
// what a store has to produce for a cluster to read.
package statement

// Claims is the payload of a signed statement.
type Claims struct {
	// Issuer identifies the store; informational, since trust is in the key.
	Issuer string `json:"iss,omitempty"`
	// Audience is "cluster:<id>": a statement for another cluster is not one
	// for this cluster, whoever signed it.
	Audience string `json:"aud"`
	// Subject is "tenant:<name>".
	Subject string `json:"sub"`
	// ID identifies the statement in the store's own records.
	ID string `json:"jti"`
	// IssuedAt orders statements about the same entry.
	IssuedAt int64 `json:"iat"`
	// Expiry ends a grant. Required on a grant, ignored on a revocation.
	Expiry int64 `json:"exp,omitempty"`
	// Coordinate is the catalogue entry, <catalogue>/<app>.
	Coordinate string `json:"coordinate"`
	// Granted is false for a revocation or a denial.
	Granted bool `json:"granted"`
	// Reason is required when Granted is false: silence is not an answer a
	// cluster can log.
	Reason string `json:"reason,omitempty"`
	Seats  int    `json:"seats,omitempty"`
}
