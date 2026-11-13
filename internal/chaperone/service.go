/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
)

// Service is one chaperone: what answers its questions, and the tenant it
// serves. Credentials may be nil; then no call is made for a present person.
type Service struct {
	Rights      Rights
	Tools       Tools
	Adapters    Adapters
	Credentials Credentials
	Journal     Journal
	Tenant      string
}

// Digest is the fingerprint of a call's arguments: the SHA-256 of their JSON
// with sorted keys and no spaces.
func Digest(arguments map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if arguments == nil {
		arguments = map[string]any{}
	}
	// A map is written with its keys sorted; what a request's body can
	// hold always encodes.
	_ = enc.Encode(arguments)
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return hex.EncodeToString(sum[:])
}

// permitted is the tool, if this pass and the person's own rights allow
// calling it.
func (s *Service) permitted(ctx context.Context, pass Pass, key string) (Tool, error) {
	if !slices.Contains(pass.Tools, key) {
		return Tool{}, refuse("the run did not declare this tool")
	}
	tool, ok := s.Tools.Lookup(ctx, key)
	if !ok {
		return Tool{}, refuse("no installed adapter offers this tool")
	}
	if tool.Class != ClassRead {
		// Changing tools need the stages (preview, apply, on a leash,
		// free), and none of them is built. Until one is, nothing that
		// changes data passes, whatever a mandate says.
		return Tool{}, refuse("this chaperone lets reading tools through only")
	}
	if err := s.mayUse(ctx, pass.Person, tool.App); err != nil {
		return Tool{}, err
	}
	return tool, nil
}

func (s *Service) mayUse(ctx context.Context, person, app string) error {
	allowed, err := s.Rights.MayUse(ctx, person, app)
	if err != nil {
		return errors.Join(ErrRightsUnavailable, err)
	}
	if !allowed {
		return refuse("this person may not use %s", app)
	}
	return nil
}

// Offered are the tools this run may call right now.
func (s *Service) Offered(ctx context.Context, pass Pass) ([]Tool, error) {
	found := []Tool{}
	for _, key := range pass.Tools {
		tool, err := s.permitted(ctx, pass, key)
		if errors.Is(err, ErrRightsUnavailable) {
			return nil, err
		}
		if err == nil {
			found = append(found, tool)
		}
	}
	return found, nil
}

// Call is one call of a run. The line is written whatever happened, and
// before the caller hears.
func (s *Service) Call(ctx context.Context, pass Pass, key string, arguments map[string]any, now int64) (any, error) {
	line := Line{
		At: now, Tenant: s.Tenant, Person: pass.Person, Engine: pass.Engine,
		Fingerprint: pass.Fingerprint, Run: pass.Run, Mandate: pass.Mandate,
		Tool: key, Stage: pass.Stage, Decision: "refused", ArgumentsDigest: Digest(arguments),
	}
	result, err := s.carry(ctx, &line, func() (Tool, error) { return s.permitted(ctx, pass, key) }, func(tool Tool) (any, error) {
		// A call for a person who is away needs a credential obtained by
		// vouching, which is not built: the adapters are asked without
		// one, and make no call without one.
		return s.Adapters.Call(ctx, tool, arguments, "")
	})
	return s.written(ctx, line, result, err)
}

// permittedPresent is the tool, if a person who is present may have a
// program call it.
func (s *Service) permittedPresent(ctx context.Context, who Present, key string) (Tool, error) {
	tool, ok := s.Tools.Lookup(ctx, key)
	if !ok {
		return Tool{}, refuse("no installed adapter offers this tool")
	}
	if tool.Class != ClassRead && tool.Class != ClassChange {
		// What cannot be taken back needs the person's yes to that one
		// call, and there is no page for it yet.
		return Tool{}, refuse("this tool cannot be undone, and a program may not call it yet")
	}
	if err := s.mayUse(ctx, who.Person, tool.App); err != nil {
		return Tool{}, err
	}
	return tool, nil
}

// OfferedPresent are the tools a program may call for this person right now.
func (s *Service) OfferedPresent(ctx context.Context, who Present) ([]Tool, error) {
	found := []Tool{}
	for _, tool := range s.Tools.All(ctx) {
		permitted, err := s.permittedPresent(ctx, who, tool.Key())
		if errors.Is(err, ErrRightsUnavailable) {
			return nil, err
		}
		if err == nil {
			found = append(found, permitted)
		}
	}
	return found, nil
}

// CallPresent is one call for a person who is present. The journal names the
// program as the one that acted and the person as the one it acted for;
// there is no run, no mandate and no graph.
func (s *Service) CallPresent(ctx context.Context, who Present, key string, arguments map[string]any, now int64) (any, error) {
	line := Line{
		At: now, Tenant: s.Tenant, Person: who.Person, Engine: who.Client,
		Tool: key, Decision: "refused", ArgumentsDigest: Digest(arguments),
	}
	result, err := s.carry(ctx, &line, func() (Tool, error) {
		tool, err := s.permittedPresent(ctx, who, key)
		if err != nil {
			return tool, err
		}
		line.Class = tool.Class
		if s.Credentials == nil {
			return tool, refuse("this chaperone cannot obtain a credential for the app")
		}
		return tool, nil
	}, func(tool Tool) (any, error) {
		credential, err := s.Credentials.ForApp(ctx, who.Token, tool.App)
		if err != nil {
			return nil, err
		}
		return s.Adapters.Call(ctx, tool, arguments, credential)
	})
	return s.written(ctx, line, result, err)
}

// carry decides and, if allowed, makes the call, and fills the line in.
func (s *Service) carry(_ context.Context, line *Line, decide func() (Tool, error), call func(Tool) (any, error)) (any, error) {
	tool, err := decide()
	if tool.Class != "" {
		line.Class = tool.Class
	}
	if err != nil {
		line.Reason = err.Error()
		var refused *Refused
		if errors.As(err, &refused) {
			line.Reason = refused.Reason
		}
		return nil, err
	}
	line.Decision = "allowed"
	result, err := call(tool)
	if err != nil {
		line.Outcome, line.Reason = "error", err.Error()
		return nil, err
	}
	line.Outcome = "ok"
	return result, nil
}

// written writes the line. A call whose line could not be written is not
// answered as a success: what happened and was not recorded is said so.
func (s *Service) written(ctx context.Context, line Line, result any, err error) (any, error) {
	// The line is kept even when the caller has gone.
	if jerr := s.Journal.Add(context.WithoutCancel(ctx), line); jerr != nil {
		return nil, errors.Join(err, adapterError("the journal could not be written: %v", jerr))
	}
	return result, err
}
