/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package chaperone carries a tool call to an app's adapter for a person.
//
// Every call a program or an agent engine makes to an app of a tenant passes
// here. The chaperone decides nothing by a rule of its own: whether a person
// may use an app is the platform's rights check; what a tool is, is the
// installed adapter's own list; a present person's token is the realm's; and
// a pass is its issuer's word that a run of a person who is away is covered.
// A question nobody answers is a refusal. Every call leaves a journal line,
// whether it was let through or not.
package chaperone

import (
	"context"
	"errors"
	"fmt"
)

// The classes of a tool. A tool with no class counts as irreversible.
const (
	ClassRead         = "read"
	ClassChange       = "change"
	ClassIrreversible = "irreversible"
)

// Tool is a tool as its adapter describes it.
type Tool struct {
	Adapter     string
	Name        string
	Version     int
	App         string
	Class       string
	Description string
	// InputSchema is what the tool takes, as a JSON Schema, for a program
	// that has to be told. No part of what the tool is: two tools are the
	// same by their key.
	InputSchema map[string]any
}

// Key names a tool: <adapter>/<tool>@<version>.
func (t Tool) Key() string { return fmt.Sprintf("%s/%s@%d", t.Adapter, t.Name, t.Version) }

// Rights is the platform's answer to "may this person use this app". An
// error is never an answer: the caller refuses.
type Rights interface {
	MayUse(ctx context.Context, person, app string) (bool, error)
}

// Tools are the tools the installed adapters offer.
type Tools interface {
	Lookup(ctx context.Context, key string) (Tool, bool)
	All(ctx context.Context) []Tool
}

// Present is a person who is present: they signed in and connected a
// program, and the program's call carries their own token. Client is the
// program, as the realm knows it.
type Present struct {
	Person string
	Client string
	Token  string
}

// Persons verifies the token of a person who is present. Good only if the
// tenant's realm issued it and made it out to this chaperone.
type Persons interface {
	Verify(ctx context.Context, token string, now int64) (Present, error)
}

// Credentials turns a present person's token into one valid at a single app,
// for minutes. The realm's answer, never the chaperone's own making.
type Credentials interface {
	ForApp(ctx context.Context, token, app string) (string, error)
}

// Pass is what a verified pass says.
type Pass struct {
	Person      string
	Engine      string
	Run         string
	Mandate     string
	Fingerprint string
	Stage       string
	Tools       []string
}

// Passes verifies a pass.
type Passes interface {
	Verify(ctx context.Context, token string, now int64) (Pass, error)
}

// Adapters carries one call to an adapter with the credential obtained for
// the app. An adapter holds no credential of its app.
type Adapters interface {
	Call(ctx context.Context, tool Tool, arguments map[string]any, credential string) (any, error)
}

// Line is one journal line: one tool call, allowed or refused. Arguments are
// kept as a digest: the journal says what was done for whom, not what the
// data was.
type Line struct {
	At              int64
	Tenant          string
	Person          string
	Engine          string
	Fingerprint     string
	Run             string
	Mandate         string
	Tool            string
	Class           string
	Stage           string
	Decision        string // allowed, refused
	Reason          string
	Outcome         string // ok, error
	ArgumentsDigest string
}

// Journal takes lines. They are added and never changed.
type Journal interface {
	Add(ctx context.Context, line Line) error
}

// Refused is a call the chaperone did not let through, and why.
type Refused struct{ Reason string }

func (r *Refused) Error() string { return r.Reason }

func refuse(format string, args ...any) error { return &Refused{Reason: fmt.Sprintf(format, args...)} }

// AdapterError says the adapter, its app or the realm failed. Not a refusal:
// the call was allowed.
type AdapterError struct{ Message string }

func (e *AdapterError) Error() string { return e.Message }

func adapterError(format string, args ...any) error {
	return &AdapterError{Message: fmt.Sprintf(format, args...)}
}

// ErrRightsUnavailable says the platform could not be asked about a person's
// rights. That is never a yes.
var ErrRightsUnavailable = errors.New("the platform could not be asked about this person's rights")
