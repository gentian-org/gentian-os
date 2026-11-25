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
	"fmt"
	"log/slog"
	"net/http"
)

// A driver speaks one app framework's API, as the person, and knows nothing
// of the home: it reads and writes an app's records as Values, and says when
// each was last changed. One per framework, so that fitting an app of a
// framework that has one is its profile's declaration and nothing else.
type Driver interface {
	// Person is who the person is in the app, for the placeholders of a
	// mapping's filter and defaults. person is their subject in the realm.
	Person(ctx context.Context, app *App, token, person string) (Person, error)
	// Stamps are the person's records of the mapping, each with a value
	// that changes whenever the record does.
	Stamps(ctx context.Context, app *App, m *Mapping, token string, who Person) (map[string]string, error)
	// Read is one record's parts and stamp.
	Read(ctx context.Context, app *App, m *Mapping, token, id string) (Values, string, error)
	// Create makes a record of the parts, with the mapping's defaults, and
	// returns its id and stamp.
	Create(ctx context.Context, app *App, m *Mapping, token string, who Person, v Values) (string, string, error)
	// Update writes the parts onto a record and returns its id and new
	// stamp. The id is a new one where the app had to make the record again
	// (Odoo's series whose rule changed).
	Update(ctx context.Context, app *App, m *Mapping, token string, who Person, id string, v Values) (string, string, error)
	// Delete removes a record. One already gone is gone.
	Delete(ctx context.Context, app *App, m *Mapping, token, id string) error
}

// Person is who the person is in an app: the values its placeholders stand
// for.
type Person map[string]any

// Drivers are the drivers by name, as a mapping names them.
func Drivers(client *http.Client, log *slog.Logger) map[string]Driver {
	return map[string]Driver{"odoo": &Odoo{Client: client, Log: log}}
}

func driverFor(drivers map[string]Driver, m *Mapping) (Driver, error) {
	d := drivers[m.Driver]
	if d == nil {
		return nil, fmt.Errorf("no driver %q", m.Driver)
	}
	return d, nil
}
