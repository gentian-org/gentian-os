/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package porter is the platform's file dialog (gentian-os#1200). An app's
// page asks it for a file; the person chooses in a window of the porter's
// own, at the porter's address, and the page is handed what they chose and
// nothing else. No app is given a person's token for the tenant's file home.
//
// One instance runs per tenant. It lists a person's files at the home over
// WebDAV as that person, with a token it obtains from the token the edge
// hands it for the person at its address (the realm's token exchange), and
// so for nobody who is not there.
package porter

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Use is one thing an app asks the dialog for.
type Use string

// The uses an app declares (integrations[].dialog).
const (
	Pick Use = "pick"
	Open Use = "open"
	Save Use = "save"
)

// Plan is what the porter serves: the tenant's file home and the apps that
// may ask. The platform writes it as JSON, and it is read again when it is
// used.
type Plan struct {
	// Home is the tenant's file home, absent while the tenant has none that
	// says where a person's files are.
	Home *Home `json:"home,omitempty"`
	// Apps are the apps granted the file store that ask the dialog.
	Apps []App `json:"apps"`
}

// Home is the tenant's file home, as its profile declares it.
type Home struct {
	// Profile is the app's profile.
	Profile string `json:"profile"`
	// URL is where it answers WebDAV inside the tenant.
	URL string `json:"url"`
	// Root is where a person's files start, below URL; "{principal}" stands
	// for the last part of the person's principal address.
	Root string `json:"root"`
	// IDProperty is the WebDAV property that holds a file's id,
	// "{namespace}name".
	IDProperty string `json:"idProperty"`
	// Open is the address that opens a file by its id in a browser, with
	// "{id}" where the id goes.
	Open string `json:"open"`
	// Scope is what the porter asks the realm for, for a token valid at the
	// home: the scope the realm keeps for this home's tokens, which it
	// grants the porter's client.
	Scope string `json:"scope"`
}

// App is one app that may ask.
type App struct {
	Profile string `json:"profile"`
	// Origins are the origins of the app's pages (https://host).
	Origins []string `json:"origins"`
	// Uses are what it may ask for.
	Uses []Use `json:"uses"`
}

// ReadPlan reads the plan the platform wrote. A plan not written yet is an
// empty one: no home, nobody may ask.
func ReadPlan(path string) (*Plan, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // The path is the service's own setting.
	if os.IsNotExist(err) {
		return &Plan{}, nil
	}
	if err != nil {
		return nil, err
	}
	p := &Plan{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, fmt.Errorf("the plan at %s is not JSON of a plan: %w", path, err)
	}
	return p, p.validate()
}

func (p *Plan) validate() error {
	if h := p.Home; h != nil {
		if h.Profile == "" || h.URL == "" || h.Root == "" || h.IDProperty == "" || h.Scope == "" {
			return fmt.Errorf("the home of the plan names no profile, address, root, id property or scope")
		}
		if _, _, err := splitProperty(h.IDProperty); err != nil {
			return err
		}
		if !strings.Contains(h.Open, "{id}") {
			return fmt.Errorf("the home's open address has no {id}")
		}
		if u, err := url.Parse(strings.ReplaceAll(h.Open, "{id}", "0")); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("the home's open address is not an https address")
		}
	}
	for _, a := range p.Apps {
		if a.Profile == "" || len(a.Origins) == 0 {
			return fmt.Errorf("an app of the plan names no profile or no origin")
		}
	}
	return nil
}

// AppOf is the app whose page is at the origin and may ask for the use, or
// nil.
func (p *Plan) AppOf(origin string, use Use) *App {
	for i := range p.Apps {
		a := &p.Apps[i]
		for _, o := range a.Origins {
			if o != origin {
				continue
			}
			for _, u := range a.Uses {
				if u == use {
					return a
				}
			}
		}
	}
	return nil
}

// splitProperty reads "{namespace}name".
func splitProperty(p string) (space, name string, err error) {
	end := strings.Index(p, "}")
	if !strings.HasPrefix(p, "{") || end < 2 || end == len(p)-1 {
		return "", "", fmt.Errorf("the id property %q is not written {namespace}name", p)
	}
	return p[1:end], p[end+1:], nil
}
