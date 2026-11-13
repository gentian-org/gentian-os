/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// IssuerPasses verifies a pass against the keys its issuer publishes. Who
// the issuer is, is a setting of the install: the chaperone knows it by its
// name in the pass and by where its keys are.
type IssuerPasses struct {
	keys     *keySet
	issuer   string
	audience string
	tenant   string
}

// NewIssuerPasses verifies passes of issuer, made out to audience for tenant,
// against the key set at keysURL.
func NewIssuerPasses(keysURL, issuer, audience, tenant string, client *http.Client) *IssuerPasses {
	return &IssuerPasses{keys: newKeySet(keysURL, client), issuer: issuer, audience: audience, tenant: tenant}
}

type passClaims struct {
	Tenant      string   `json:"tenant"`
	Engine      string   `json:"engine"`
	Run         string   `json:"run"`
	Mandate     string   `json:"mandate"`
	Fingerprint string   `json:"fingerprint"`
	Stage       string   `json:"stage"`
	Tools       []string `json:"tools"`
	RunEnds     int64    `json:"run_ends"`
}

func (p *IssuerPasses) Verify(ctx context.Context, token string, now int64) (Pass, error) {
	var std jwt.Claims
	var own passClaims
	if err := p.keys.verified(ctx, token, []jose.SignatureAlgorithm{jose.ES256}, &std, &own); err != nil {
		return Pass{}, fmt.Errorf("the pass is not good: %w", err)
	}
	if err := standard(std, p.issuer, p.audience, now); err != nil {
		return Pass{}, fmt.Errorf("the pass is not good: %w", err)
	}
	return checkPass(std.Subject, own, p.tenant, now)
}

// checkPass is what a genuine pass must still say to be good here.
func checkPass(person string, c passClaims, tenant string, now int64) (Pass, error) {
	if c.Tenant != tenant {
		return Pass{}, fmt.Errorf("the pass is for another tenant")
	}
	for _, claim := range [][2]string{{"engine", c.Engine}, {"run", c.Run}, {"mandate", c.Mandate}, {"fingerprint", c.Fingerprint}} {
		if claim[1] == "" {
			return Pass{}, fmt.Errorf("the pass names no %s", claim[0])
		}
	}
	if len(c.Tools) == 0 {
		return Pass{}, fmt.Errorf("the pass names no tools")
	}
	if c.RunEnds <= now {
		return Pass{}, fmt.Errorf("the run has reached its time limit")
	}
	return Pass{
		Person: person, Engine: c.Engine, Run: c.Run, Mandate: c.Mandate,
		Fingerprint: c.Fingerprint, Stage: c.Stage, Tools: c.Tools,
	}, nil
}
