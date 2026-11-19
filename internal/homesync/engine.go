/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Engine runs the rounds. Each round reads the plan, asks who is linked to
// the service, and keeps, for every one of them, every app's records of every
// kind it declares equal to the home's entries of that kind.
//
// The rules, the same for every app:
//
//   - A pair whose entry changed at the home takes the home's version, even
//     where the record changed too: the home wins.
//   - A pair whose record changed, and whose entry did not, takes the
//     record's version. The write names the entry's version, so a change at
//     the home meanwhile is not written over; the next round takes it.
//   - A side that lost its half loses the other: a deletion goes both ways.
//     The one exception is the home's rule again: an entry changed at the
//     home whose record was deleted in the app is made again in the app.
//   - An entry or a record with no pair is new, and is made on the other side.
type Engine struct {
	Plan   func() (*Plan, error)
	People func(ctx context.Context) ([]string, error)
	Tokens interface {
		For(ctx context.Context, person, scope string) (string, error)
	}
	Store   Store
	Drivers map[string]Driver
	Client  *http.Client
	Log     *slog.Logger
}

// Scopes are the realm scopes a token for the home and for an app is asked
// with: what HomeTokenScope and AppTokenScope in the operator name.
func homeScope(profile string) string { return "home-" + profile }
func appScope(profile string) string  { return "app-" + profile }

// Round is one pass over everybody and everything.
func (e *Engine) Round(ctx context.Context) error {
	plan, err := e.Plan()
	if err != nil {
		return fmt.Errorf("read the plan: %w", err)
	}
	if plan.Home.Profile == "" || len(plan.Apps) == 0 {
		return nil
	}
	people, err := e.People(ctx)
	if err != nil {
		return fmt.Errorf("ask who is linked: %w", err)
	}
	for _, person := range people {
		homeToken, err := e.Tokens.For(ctx, person, homeScope(plan.Home.Profile))
		if err != nil {
			e.skip(person, "the home", err)
			continue
		}
		for i := range plan.Apps {
			app := &plan.Apps[i]
			appToken, err := e.Tokens.For(ctx, person, appScope(app.Profile))
			if err != nil {
				e.skip(person, app.Profile, err)
				continue
			}
			for _, kind := range []Kind{Calendar, Contacts} {
				m, ok := app.Kinds[kind]
				if !ok {
					continue
				}
				root := plan.Home.Calendar
				if kind == Contacts {
					root = plan.Home.Contacts
					if plan.ContactsBook == "shared" {
						e.Log.Warn("a shared address book is not kept yet; contacts are not synced", "app", app.Profile)
						continue
					}
				}
				if root == "" {
					continue
				}
				dav := &DAV{Root: root, Kind: kind, Token: homeToken, Client: e.Client}
				if err := e.sync(ctx, person, app, kind, &m, dav, appToken); err != nil {
					e.Log.Warn("sync failed", "person", person, "app", app.Profile, "kind", kind, "error", err.Error())
				}
			}
		}
	}
	return nil
}

func (e *Engine) skip(person, what string, err error) {
	if errors.Is(err, ErrNotLinked) {
		e.Log.Debug("no token", "person", person, "for", what, "error", err.Error())
		return
	}
	e.Log.Warn("no token", "person", person, "for", what, "error", err.Error())
}

// sync keeps one person's records of one kind in one app equal to the home.
func (e *Engine) sync(ctx context.Context, person string, app *App, kind Kind, m *Mapping, dav *DAV, appToken string) error {
	drv, err := driverFor(e.Drivers, m)
	if err != nil {
		return err
	}
	collection, err := dav.Collection(ctx)
	if err != nil {
		return err
	}
	home, err := dav.List(ctx, collection)
	if err != nil {
		return err
	}
	who, err := drv.Person(ctx, app, appToken, person)
	if err != nil {
		return err
	}
	records, err := drv.Stamps(ctx, app, m, appToken, who)
	if err != nil {
		return err
	}
	pairs, err := e.Store.Pairs(ctx, person, app.Profile, kind)
	if err != nil {
		return err
	}

	paired := map[string]bool{}
	recorded := map[string]bool{}
	for _, p := range pairs {
		paired[p.Href] = true
		recorded[p.AppID] = true
		etag, inHome := home[p.Href]
		stamp, inApp := records[p.AppID]
		switch {
		case !inHome && !inApp:
			err = e.Store.Remove(ctx, p)
		case !inHome:
			if err = drv.Delete(ctx, app, m, appToken, p.AppID); err == nil {
				err = e.Store.Remove(ctx, p)
			}
		case !inApp && etag != p.ETag:
			// Changed at the home, deleted in the app: the home wins.
			err = e.fromHome(ctx, drv, app, m, appToken, who, dav, p, "")
		case !inApp:
			if err = dav.Delete(ctx, p.Href, p.ETag); err == nil || errors.Is(err, ErrChanged) {
				err = e.Store.Remove(ctx, p)
			}
		case etag != p.ETag:
			err = e.fromHome(ctx, drv, app, m, appToken, who, dav, p, p.AppID)
		case stamp != p.Stamp:
			err = e.fromApp(ctx, drv, app, m, appToken, dav, p)
		}
		if err != nil {
			e.Log.Warn("a pair was not kept", "person", person, "app", app.Profile, "kind", kind, "record", p.AppID, "error", err.Error())
		}
	}
	for href := range home {
		if paired[href] {
			continue
		}
		p := Pair{Person: person, App: app.Profile, Kind: kind, Href: href}
		if err := e.fromHome(ctx, drv, app, m, appToken, who, dav, p, ""); err != nil && !errors.Is(err, errNotAnEntry) {
			e.Log.Warn("an entry of the home was not made in the app", "person", person, "app", app.Profile, "kind", kind, "entry", href, "error", err.Error())
		}
	}
	for id := range records {
		if recorded[id] {
			continue
		}
		p := Pair{Person: person, App: app.Profile, Kind: kind, AppID: id}
		if err := e.toHome(ctx, drv, app, m, appToken, dav, collection, p); err != nil {
			e.Log.Warn("a record was not made at the home", "person", person, "app", app.Profile, "kind", kind, "record", id, "error", err.Error())
		}
	}
	return nil
}

// fromHome writes the home's entry onto the app: onto the record id, or as a
// new record where id is "".
func (e *Engine) fromHome(ctx context.Context, drv Driver, app *App, m *Mapping, token string, who Person, dav *DAV, p Pair, id string) error {
	body, etag, err := dav.Get(ctx, p.Href)
	if err != nil {
		return err
	}
	v, _, err := Read(p.Kind, body)
	if err != nil {
		return err
	}
	if etag == "" {
		listed, err := dav.List(ctx, parentOf(p.Href))
		if err == nil {
			etag = listed[p.Href]
		}
	}
	stamp := ""
	if id == "" {
		id, stamp, err = drv.Create(ctx, app, m, token, who, only(v, m))
	} else {
		stamp, err = drv.Update(ctx, app, m, token, id, only(v, m))
		if errors.Is(err, errGone) {
			id, stamp, err = drv.Create(ctx, app, m, token, who, only(v, m))
		}
	}
	if err != nil {
		return err
	}
	p.AppID, p.Stamp, p.ETag = id, stamp, etag
	return e.Store.Save(ctx, p)
}

// fromApp writes the app's record onto the home's entry.
func (e *Engine) fromApp(ctx context.Context, drv Driver, app *App, m *Mapping, token string, dav *DAV, p Pair) error {
	v, stamp, err := drv.Read(ctx, app, m, token, p.AppID)
	if err != nil {
		return err
	}
	body, current, err := dav.Get(ctx, p.Href)
	if err != nil {
		return err
	}
	if current != "" && current != p.ETag {
		return nil // changed at the home meanwhile: the next round takes it
	}
	written, err := Write(p.Kind, body, "", v)
	if err != nil {
		return err
	}
	etag, err := dav.Put(ctx, p.Href, p.ETag, written)
	if errors.Is(err, ErrChanged) {
		return nil
	}
	if err != nil {
		return err
	}
	if etag == "" {
		if _, etag, err = dav.Get(ctx, p.Href); err != nil {
			return err
		}
	}
	p.Stamp, p.ETag = stamp, etag
	return e.Store.Save(ctx, p)
}

// toHome makes a new entry at the home of the app's record.
func (e *Engine) toHome(ctx context.Context, drv Driver, app *App, m *Mapping, token string, dav *DAV, collection string, p Pair) error {
	v, stamp, err := drv.Read(ctx, app, m, token, p.AppID)
	if err != nil {
		return err
	}
	uid, err := newUID()
	if err != nil {
		return err
	}
	body, err := Write(p.Kind, nil, uid, v)
	if err != nil {
		return err
	}
	href := dav.NewHref(collection, uid)
	etag, err := dav.Put(ctx, href, "", body)
	if err != nil {
		return err
	}
	if etag == "" {
		if _, etag, err = dav.Get(ctx, href); err != nil {
			return err
		}
	}
	p.Href, p.Stamp, p.ETag = href, stamp, etag
	return e.Store.Save(ctx, p)
}

// only is the parts the mapping keeps: a part the app has no field for is not
// written, and so not emptied there.
func only(v Values, m *Mapping) Values {
	out := Values{}
	for part := range m.Fields {
		if s, ok := v[part]; ok {
			out[part] = s
		}
	}
	return out
}

func parentOf(href string) string {
	for i := len(href) - 1; i > 0; i-- {
		if href[i] == '/' {
			return href[:i+1]
		}
	}
	return "/"
}

func newUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "gentian-" + hex.EncodeToString(b), nil
}
