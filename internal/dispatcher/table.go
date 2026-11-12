/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package dispatcher is what the operator and the dispatcher agree on: the
// table the operator writes and the dispatcher reads, the labels a run's pod
// carries and the tenant's network rules select by, and what a run is told in
// its environment. Both sides import these, so neither can name another.
package dispatcher

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
)

const (
	// TableConfigMap is the operator's table, in the dispatcher's namespace,
	// under TableKey.
	TableConfigMap = "dispatcher-runs"
	TableKey       = "runs.json"

	// Port is the listener components call. The director's acts are on
	// ActsPort (acts.go).
	Port = int32(8080)

	// ServiceAccount is the ServiceAccount a run's pod runs under, in the
	// tenant's namespace for runs. It holds no right and mounts no token.
	ServiceAccount = "run"

	// LabelRun marks a run's pod. Its value is the hash a run is found by.
	LabelRun = "gentianos.io/run"
	// LabelCaller is the component that started the run. The way from a
	// run's pod to that component, and no other, is selected by it.
	LabelCaller = "gentianos.io/run-caller"
	// LabelAction is the installed action a run is of. The egress granted to
	// that action is selected by it. A run of the caller's own has none.
	LabelAction = "gentianos.io/run-action"

	// URLKey and KeyKey are the entries of the Secret a component that
	// declares requires.services.runs is given.
	URLKey = "RUNS_URL"
	KeyKey = "RUNS_KEY" //nolint:gosec // Secret key name, not a credential.

	// RuntimeContainer runs an image as a pod per attempt.
	RuntimeContainer = "container"
)

// Runnable is one thing that can be run: an installed action, or a run of a
// caller's own.
type Runnable struct {
	// Name is what a caller asks for: the action's profile, or the name of
	// the caller's own run.
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Image and Digest are the one build that runs.
	Image   string   `json:"image"`
	Digest  string   `json:"digest"`
	Command []string `json:"command"`
	Runtime string   `json:"runtime"`
	// CPU and Memory are what one attempt reserves and may use, as
	// quantities. Empty means the namespace's default.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// DeadlineSeconds is the longest one attempt may take.
	DeadlineSeconds int32 `json:"deadlineSeconds"`
	// PullSecrets are the Secrets of the tenant's namespace for runs the
	// image is pulled with, by name: the cluster's own, then those of the
	// repositories the tenant declared.
	PullSecrets []string `json:"pullSecrets,omitempty"`
}

// Ref is the image a pod names: the repository at the digest.
func (r Runnable) Ref() string { return r.Image + "@" + r.Digest }

// Callback is where a run reaches the component that started it.
type Callback struct {
	Service   string `json:"service"`
	Namespace string `json:"namespace"`
	Port      int32  `json:"port"`
	Path      string `json:"path,omitempty"`
}

// URL is the address a run is told.
func (c Callback) URL() string {
	return fmt.Sprintf("http://%s.%s.svc:%d%s", c.Service, c.Namespace, c.Port, c.Path)
}

// Caller is one component that may start runs, by the hash of its key.
type Caller struct {
	componentkeys.Key
	Callback Callback `json:"callback"`
	// Concurrency is the most runs the caller has under way at once.
	Concurrency int32 `json:"concurrency"`
	// Own are the caller's own runs, which no other caller starts.
	Own []Runnable `json:"own,omitempty"`
}

// Tenant is what may run in one tenant, and where.
type Tenant struct {
	Tenant string `json:"tenant"`
	// Namespace is the tenant's namespace for runs.
	Namespace string `json:"namespace"`
	// Actions are the tenant's installed actions. Every caller of the tenant
	// may run each of them.
	Actions []Runnable `json:"actions,omitempty"`
}

// Table is what the operator writes: who may start runs, and what may run.
type Table struct {
	Callers []Caller `json:"callers"`
	Tenants []Tenant `json:"tenants"`
}

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	digest   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// secretName is a Secret's name: labels joined by dots, 253 at most.
	secretName = regexp.MustCompile(`^[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?$`)
)

func (r Runnable) check() error {
	switch {
	case !dnsLabel.MatchString(r.Name):
		return fmt.Errorf("%q is not a name", r.Name)
	case r.Image == "" || !digest.MatchString(r.Digest):
		return fmt.Errorf("%s names no image at a digest", r.Name)
	case len(r.Command) == 0:
		return fmt.Errorf("%s names no command", r.Name)
	case r.Runtime != RuntimeContainer:
		return fmt.Errorf("%s is for the runtime %q, which the dispatcher does not have", r.Name, r.Runtime)
	case r.DeadlineSeconds < 1:
		return fmt.Errorf("%s has no deadline", r.Name)
	}
	for _, secret := range r.PullSecrets {
		if !secretName.MatchString(secret) {
			return fmt.Errorf("%s is pulled with %q, which is not a Secret's name", r.Name, secret)
		}
	}
	return nil
}

func unique(what string, runnables []Runnable) error {
	seen := map[string]bool{}
	for _, r := range runnables {
		if err := r.check(); err != nil {
			return err
		}
		if seen[r.Name] {
			return fmt.Errorf("%s names %s twice", what, r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// ParseTable reads the table the operator wrote. An entry that is not one
// refuses the whole table: a table the operator did not write is not one to
// start pods by.
func ParseTable(raw []byte) (*Table, error) {
	t := &Table{}
	if err := json.Unmarshal(raw, t); err != nil {
		return nil, fmt.Errorf("the dispatcher's table: %w", err)
	}
	tenants := map[string]bool{}
	for _, tenant := range t.Tenants {
		if !dnsLabel.MatchString(tenant.Tenant) || !dnsLabel.MatchString(tenant.Namespace) || tenants[tenant.Tenant] {
			return nil, fmt.Errorf("the dispatcher's table holds a tenant that is not one, or twice: %q", tenant.Tenant)
		}
		tenants[tenant.Tenant] = true
		if err := unique("tenant "+tenant.Tenant, tenant.Actions); err != nil {
			return nil, fmt.Errorf("the dispatcher's table: %w", err)
		}
	}
	seen := map[string]bool{}
	for _, c := range t.Callers {
		who := c.Tenant + "/" + c.Component
		if !dnsLabel.MatchString(c.Tenant) || !dnsLabel.MatchString(c.Component) || len(c.KeyHash) != 64 {
			return nil, fmt.Errorf("the dispatcher's table holds a caller that is not one: %q", who)
		}
		if seen[c.KeyHash] || seen[who] {
			return nil, fmt.Errorf("the dispatcher's table names %s or its key twice", who)
		}
		seen[c.KeyHash], seen[who] = true, true
		if !tenants[c.Tenant] {
			return nil, fmt.Errorf("the dispatcher's table names the caller %s and no tenant for it", who)
		}
		if !dnsLabel.MatchString(c.Callback.Service) || !dnsLabel.MatchString(c.Callback.Namespace) ||
			c.Callback.Port < 1 || c.Callback.Port > 65535 || c.Concurrency < 1 {
			return nil, fmt.Errorf("the dispatcher's table gives %s no callback or no limit", who)
		}
		if err := unique(who, c.Own); err != nil {
			return nil, fmt.Errorf("the dispatcher's table: %w", err)
		}
	}
	return t, nil
}

// Document is the table as the ConfigMap holds it. Nil lists are written
// empty, so that a table with nothing in it is still a table.
func (t Table) Document() (string, error) {
	if t.Callers == nil {
		t.Callers = []Caller{}
	}
	if t.Tenants == nil {
		t.Tenants = []Tenant{}
	}
	raw, err := json.Marshal(t)
	return string(raw), err
}

// CallerByKey is the caller a presented key belongs to, nil when none.
func (t *Table) CallerByKey(key string) *Caller {
	if t == nil || key == "" {
		return nil
	}
	// Every entry is compared, in constant time each, so that how long the
	// answer takes says nothing about whose key came close.
	hash := []byte(componentkeys.Hash(key))
	var found *Caller
	for i := range t.Callers {
		if subtle.ConstantTimeCompare(hash, []byte(t.Callers[i].KeyHash)) == 1 {
			found = &t.Callers[i]
		}
	}
	return found
}

// TenantOf is the tenant's entry, nil when the table has none.
func (t *Table) TenantOf(name string) *Tenant {
	if t == nil {
		return nil
	}
	for i := range t.Tenants {
		if t.Tenants[i].Tenant == name {
			return &t.Tenants[i]
		}
	}
	return nil
}

// Runnable is what the caller means by a name: its own run of that name, or
// else the tenant's action of that name. The bool says whether it is the
// caller's own.
func (t *Table) Runnable(c *Caller, name string) (Runnable, bool, bool) {
	for _, own := range c.Own {
		if own.Name == name {
			return own, true, true
		}
	}
	if tenant := t.TenantOf(c.Tenant); tenant != nil {
		for _, action := range tenant.Actions {
			if action.Name == name {
				return action, false, true
			}
		}
	}
	return Runnable{}, false, false
}
