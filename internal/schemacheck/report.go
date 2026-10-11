/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// What the cluster was found to serve of one definition.
const (
	// StateOK is a definition served with every field the binary was built
	// with.
	StateOK = "ok"
	// StateOutdated is a definition that would prune a field: the cluster's
	// copy is older than the software.
	StateOutdated = "outdated"
	// StateAbsent is a definition the cluster does not have. Writing the
	// kind fails outright instead of losing fields, so this is "not ready",
	// not "outdated": Crossplane generates a CRD some time after its XRD is
	// applied, and the installer applies XRDs before the operator exists.
	StateAbsent = "absent"
	// StateUnreadable is a definition the operator is not permitted to read.
	// Its permission to read definitions comes with the chart, so this is a
	// chart older than the operator -- the very thing being looked for.
	StateUnreadable = "unreadable"
	// StateUnknown is a definition that could not be read for any other
	// reason and has not been read before.
	StateUnknown = "unknown"
)

// Reasons a reconciler holds or a write is refused. They are condition
// reasons and Event reasons, so they are one word each.
const (
	// ReasonOutdated: a definition on the cluster would prune what is written.
	ReasonOutdated = "DefinitionsOutdated"
	// ReasonNotReady: a definition is not on the cluster yet.
	ReasonNotReady = "DefinitionsNotReady"
	// ReasonUnconfirmed: what the cluster serves could not be established.
	ReasonUnconfirmed = "DefinitionsUnconfirmed"
)

// ConditionType is the condition a held object carries. It is present only
// while the object is held, with status False.
const ConditionType = "DefinitionsCurrent"

// KindReport is what was found for one definition.
type KindReport struct {
	Kind   string `json:"kind"`
	CRD    string `json:"crd"`
	Source string `json:"source"`
	// XRD is the CompositeResourceDefinition the CRD is generated from, for
	// a SourceXRD definition.
	XRD   string `json:"xrd,omitempty"`
	State string `json:"state"`
	// Missing is the field paths the cluster would prune, for StateOutdated.
	Missing []string `json:"missing,omitempty"`
	// Detail says what could not be read, for the states that are not a
	// comparison.
	Detail string `json:"detail,omitempty"`
	// Remedy is what a person does about a state that is not StateOK.
	Remedy string `json:"remedy,omitempty"`
}

// Report is one check of the cluster against the binary's definitions. It is
// what the operator publishes and what the director reads.
type Report struct {
	// OK is true when every definition is served with every field.
	OK bool `json:"ok"`
	// Checked is false until the cluster has been read once. A report that
	// was never checked says nothing about any kind.
	Checked bool `json:"checked"`
	// CheckedAt is when the cluster was last read, RFC 3339.
	CheckedAt string `json:"checkedAt,omitempty"`
	// Version is the build of the software that checked, when it knows one.
	Version string `json:"version,omitempty"`
	// Definitions identifies the set of definitions that software was built
	// with (Set.Digest). A reader built with another set cannot use this
	// report for its own writes: its fields are not the ones checked.
	Definitions string       `json:"definitions"`
	Kinds       []KindReport `json:"kinds"`
}

// Kind returns the report for one kind.
func (r *Report) Kind(kind string) (KindReport, bool) {
	for _, k := range r.Kinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return KindReport{}, false
}

// NotOK is the kinds whose state is not StateOK.
func (r *Report) NotOK() []KindReport {
	var out []KindReport
	for _, k := range r.Kinds {
		if k.State != StateOK {
			out = append(out, k)
		}
	}
	return out
}

// Fingerprint is equal for two reports that say the same thing about the
// cluster, whenever they were made. It is how a change is told from a repeat.
func (r *Report) Fingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%t %t\n", r.Checked, r.OK)
	for _, k := range r.Kinds {
		fmt.Fprintf(&b, "%s %s %s %s\n", k.CRD, k.State, strings.Join(k.Missing, ","), k.Detail)
	}
	return b.String()
}

// fieldList renders field paths for a sentence: all of a short list, the
// first few of a long one.
func fieldList(fields []string) string {
	const shown = 6
	if len(fields) <= shown {
		return strings.Join(fields, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(fields[:shown], ", "), len(fields)-shown)
}

// Sentence says what is wrong with one definition and what to do about it,
// for a log line, an Event, a condition or a refusal.
func (k KindReport) Sentence() string {
	switch k.State {
	case StateOutdated:
		if len(k.Missing) == 0 {
			return fmt.Sprintf("the cluster's definition of %s (%s) is older than this software: %s; %s", k.Kind, k.CRD, k.Detail, k.Remedy)
		}
		return fmt.Sprintf("the cluster's definition of %s (%s) is older than this software and would drop %s; %s",
			k.Kind, k.CRD, fieldList(k.Missing), k.Remedy)
	case StateAbsent:
		return fmt.Sprintf("the definition of %s (%s) is not on the cluster yet; %s", k.Kind, k.CRD, k.Remedy)
	case StateUnreadable:
		return fmt.Sprintf("the definition of %s (%s) may not be read, so what the cluster serves is not confirmed: %s; %s",
			k.Kind, k.CRD, k.Detail, k.Remedy)
	case StateUnknown:
		return fmt.Sprintf("the definition of %s (%s) could not be read, so what the cluster serves is not confirmed: %s", k.Kind, k.CRD, k.Detail)
	}
	return ""
}

// Hold is why something that writes must wait.
type Hold struct {
	// Reason is ReasonOutdated, ReasonNotReady or ReasonUnconfirmed.
	Reason string
	// Message names the definition, the fields and the remedy.
	Message string
	// Pending is true when nothing has been found yet: the cluster has not
	// been read once. It lasts for the moment between a process starting and
	// its first check, and is not something to tell a person about on every
	// object -- only something to wait out.
	Pending bool
}

// Gate holds the latest report, for everything in the process that has to
// act on it. The zero value is not usable; a nil *Gate holds nothing, which
// is what a process that runs no check has.
type Gate struct {
	mu     sync.RWMutex
	report Report
}

// NewGate returns a gate that has not been checked yet: everything asked of
// it is held until the first report arrives.
func NewGate(set *Set) *Gate {
	return &Gate{report: Report{Definitions: set.Digest}}
}

// Publish replaces the report.
func (g *Gate) Publish(r Report) {
	g.mu.Lock()
	g.report = r
	g.mu.Unlock()
}

// Report returns the latest report.
func (g *Gate) Report() Report {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.report
}

// Selector picks the definitions something depends on.
type Selector func(KindReport) bool

// Chart selects every definition the chart delivers. They arrive as one --
// the same Argo CD sync applies all of them -- and a reconciler of one of
// those kinds writes several of them, so one of them being older than the
// operator is the chart being older than the operator.
func Chart(k KindReport) bool { return k.Source == SourceChart }

// Kinds selects definitions by kind, and with a claim or a composite the
// other half generated from the same XRD: Crossplane copies what is written
// to one into the other, so a field pruned from either is lost.
func Kinds(set *Set, kinds ...string) Selector {
	xrds := map[string]bool{}
	named := map[string]bool{}
	for _, kind := range kinds {
		named[kind] = true
		if d, ok := set.Kind(kind); ok && d.XRD != "" {
			xrds[d.XRD] = true
		}
	}
	return func(k KindReport) bool { return named[k.Kind] || (k.XRD != "" && xrds[k.XRD]) }
}

// Hold says whether something that writes the selected definitions must
// wait, and why. Nil means it may go ahead. A nil gate never holds.
//
// The worst state among the selected definitions decides the reason:
// outdated before unconfirmed before not-ready, because that is the order in
// which a person has something to do.
func (g *Gate) Hold(selectors ...Selector) *Hold {
	if g == nil {
		return nil
	}
	r := g.Report()
	if !r.Checked {
		return &Hold{
			Pending: true,
			Reason:  ReasonUnconfirmed,
			Message: "the definitions this operator writes have not been compared with the cluster's yet; nothing is written until they are",
		}
	}
	var outdated, unconfirmed, notReady []string
	for _, k := range r.Kinds {
		selected := false
		for _, sel := range selectors {
			if sel(k) {
				selected = true
				break
			}
		}
		if !selected {
			continue
		}
		switch k.State {
		case StateOutdated:
			outdated = append(outdated, k.Sentence())
		case StateUnreadable, StateUnknown:
			unconfirmed = append(unconfirmed, k.Sentence())
		case StateAbsent:
			notReady = append(notReady, k.Sentence())
		}
	}
	for _, c := range []struct {
		reason string
		found  []string
	}{{ReasonOutdated, outdated}, {ReasonUnconfirmed, unconfirmed}, {ReasonNotReady, notReady}} {
		if len(c.found) > 0 {
			sort.Strings(c.found)
			return &Hold{Reason: c.reason, Message: summarise(c.found)}
		}
	}
	return nil
}

// summarise joins sentences into one message that stays readable as a
// condition: the first few in full, the rest counted.
func summarise(sentences []string) string {
	const shown = 3
	if len(sentences) <= shown {
		return strings.Join(sentences, ". ")
	}
	return fmt.Sprintf("%s. And %d more definitions in the same state", strings.Join(sentences[:shown], ". "), len(sentences)-shown)
}
