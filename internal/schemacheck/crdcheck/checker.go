/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package crdcheck is the operator's half of schemacheck: it reads the
// definitions the cluster serves, publishes what it found, and holds the
// reconcilers that would write into a definition that drops fields.
package crdcheck

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

const (
	// DefaultInterval is how often a cluster found in order is read again.
	// Definitions change when somebody syncs or re-runs an installer step,
	// which is rare; this bounds how long a definition that went backwards
	// under a running operator goes unnoticed.
	DefaultInterval = 5 * time.Minute
	// DefaultRetryInterval is how often a cluster found out of order is read
	// again. Short, because this is what recovery waits for: once the
	// definitions are brought up to date, held reconcilers resume within it.
	DefaultRetryInterval = 30 * time.Second
	// reminderInterval is how often an unchanged finding is logged again, so
	// that it is in the recent log and not only at the top of it.
	reminderInterval = 10 * time.Minute
	// readTimeout bounds one reading of the cluster.
	readTimeout = 30 * time.Second
)

// Checker reads the cluster's definitions on a timer and publishes the
// comparison to a Gate.
//
// A timer and not a watch. A watch would notice a change at once, but it
// needs list and watch on CustomResourceDefinitions, which a ClusterRole
// cannot restrict to named objects the way it restricts get -- the operator
// would be able to read every definition in the cluster to learn about its
// own. Reading each by name needs get on those names and nothing else, is a
// few dozen small requests every few minutes, and has no cache to keep in
// memory. What it costs is promptness in one direction only: an update that
// repairs things is picked up within the retry interval, which is short.
//
// It never returns an error for what it finds, and never stops: a cluster in
// any state is something to report and wait on, not to crash over.
type Checker struct {
	// Reader reads the API server directly (the manager's APIReader). Not
	// the cached client: that would start an informer, which is the list
	// and watch this deliberately does without.
	Reader client.Reader
	// Gate receives every report.
	Gate *schemacheck.Gate
	// Set is the definitions to require. Nil is the embedded set.
	Set *schemacheck.Set
	// Version is this build, when it is known.
	Version string
	// Recorder and EventOn say where a finding is recorded as an Event.
	// Either nil records none.
	Recorder record.EventRecorder
	EventOn  runtime.Object
	// Interval and RetryInterval default to the constants above.
	Interval      time.Duration
	RetryInterval time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	lastFingerprint string
	lastLogged      time.Time
}

func (c *Checker) set() *schemacheck.Set {
	if c.Set != nil {
		return c.Set
	}
	return schemacheck.Embedded()
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Start implements manager.Runnable: check, publish, wait, for as long as
// the manager runs.
func (c *Checker) Start(ctx context.Context) error {
	interval, retry := c.Interval, c.RetryInterval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if retry <= 0 {
		retry = DefaultRetryInterval
	}
	for {
		report := c.CheckOnce(ctx)
		wait := interval
		if !report.OK {
			wait = retry
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// NeedLeaderElection is false: every replica holds its own reconcilers and
// answers the director from its own listener, so every replica checks.
func (c *Checker) NeedLeaderElection() bool { return false }

// CheckOnce reads the cluster once, publishes the report and returns it.
func (c *Checker) CheckOnce(ctx context.Context) schemacheck.Report {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	set := c.set()
	previous := c.Gate.Report()
	report := schemacheck.Report{
		Checked:     true,
		OK:          true,
		CheckedAt:   c.now().UTC().Format(time.RFC3339),
		Version:     c.Version,
		Definitions: set.Digest,
	}
	for _, def := range set.Definitions {
		kind := c.check(ctx, def)
		if kind.State == schemacheck.StateUnknown {
			// A read that failed for a passing reason says nothing new. What
			// was last established about this definition stands, so that one
			// slow answer from the API server neither stops a healthy
			// operator nor releases a held one.
			if before, ok := previous.Kind(def.Kind); ok && previous.Checked && before.State != schemacheck.StateUnknown {
				before.Detail = kind.Detail + " (showing what was last read)"
				kind = before
			}
		}
		if kind.State != schemacheck.StateOK {
			report.OK = false
		}
		report.Kinds = append(report.Kinds, kind)
	}
	c.Gate.Publish(report)
	c.announce(ctx, report)
	return report
}

// check compares one definition with the cluster's.
func (c *Checker) check(ctx context.Context, def schemacheck.Definition) schemacheck.KindReport {
	out := schemacheck.KindReport{Kind: def.Kind, CRD: def.CRD, Source: def.Source, XRD: def.XRD}
	var crd apiextensionsv1.CustomResourceDefinition
	err := c.Reader.Get(ctx, types.NamespacedName{Name: def.CRD}, &crd)
	switch {
	case apierrors.IsNotFound(err):
		out.State = schemacheck.StateAbsent
		out.Remedy = absentRemedy(def)
		return out
	case apierrors.IsForbidden(err):
		out.State = schemacheck.StateUnreadable
		out.Detail = "the operator's ClusterRole does not let it read this definition, which a chart older than the operator would explain"
		// Whatever the definition's own source, it is the chart that grants
		// the read.
		out.Remedy = schemacheck.RemedyChart
		return out
	case err != nil:
		out.State = schemacheck.StateUnknown
		out.Detail = err.Error()
		return out
	}

	served := map[string]*apiextensionsv1.JSONSchemaProps{}
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Served && v.Schema != nil {
			served[v.Name] = v.Schema.OpenAPIV3Schema
		}
	}
	for _, version := range sortedVersions(def.Versions) {
		have, ok := served[version]
		if !ok {
			out.State = schemacheck.StateOutdated
			out.Detail = fmt.Sprintf("version %s is not served", version)
			out.Remedy = def.Remedy()
			return out
		}
		out.Missing = append(out.Missing, schemacheck.Missing(def.Versions[version], have)...)
	}
	if len(out.Missing) > 0 {
		out.State = schemacheck.StateOutdated
		out.Remedy = def.Remedy()
		return out
	}
	out.State = schemacheck.StateOK
	return out
}

// absentRemedy says how a definition that is not there comes to be there.
func absentRemedy(def schemacheck.Definition) string {
	if def.Source == schemacheck.SourceXRD {
		return "Crossplane generates it from " + def.XRD + " shortly after installer step B-06 applies that; if it stays absent, " + schemacheck.RemedyXRD
	}
	return schemacheck.RemedyChart
}

func sortedVersions(versions map[string]*apiextensionsv1.JSONSchemaProps) []string {
	out := make([]string, 0, len(versions))
	for v := range versions {
		out = append(out, v)
	}
	// One version everywhere today; sorted so that a second one cannot make
	// the report's order depend on map iteration.
	sort.Strings(out)
	return out
}

// announce logs a finding and records it as an Event -- when it changes, and
// again every reminderInterval while it stays wrong.
func (c *Checker) announce(ctx context.Context, report schemacheck.Report) {
	logger := log.FromContext(ctx).WithName("definitions")
	fingerprint := report.Fingerprint()
	changed := fingerprint != c.lastFingerprint
	if !changed && (report.OK || c.now().Sub(c.lastLogged) < reminderInterval) {
		return
	}
	first := c.lastFingerprint == ""
	c.lastFingerprint, c.lastLogged = fingerprint, c.now()

	if report.OK {
		if first {
			logger.Info("the cluster serves every field of the definitions this operator was built with",
				"definitions", len(report.Kinds), "digest", report.Definitions)
		} else {
			logger.Info("the cluster's definitions are current again; held reconcilers resume",
				"definitions", len(report.Kinds))
			c.event(corev1.EventTypeNormal, "DefinitionsCurrent",
				"the cluster serves every field of the definitions this operator was built with; held reconcilers resume")
		}
		return
	}
	for _, kind := range report.NotOK() {
		switch kind.State {
		case schemacheck.StateAbsent:
			// Not an error: this is what a cluster looks like between an XRD
			// being applied and Crossplane generating its CRD.
			logger.Info("a definition is not on the cluster yet; not ready, checking again",
				"kind", kind.Kind, "crd", kind.CRD, "source", kind.Source, "remedy", kind.Remedy)
			c.event(corev1.EventTypeWarning, schemacheck.ReasonNotReady, kind.Sentence())
		case schemacheck.StateOutdated:
			logger.Error(nil, "the cluster's definition is older than this operator and would silently drop fields",
				"kind", kind.Kind, "crd", kind.CRD, "source", kind.Source,
				"missing", kind.Missing, "detail", kind.Detail, "remedy", kind.Remedy)
			c.event(corev1.EventTypeWarning, schemacheck.ReasonOutdated, kind.Sentence())
		default:
			logger.Error(nil, "what the cluster serves of a definition could not be confirmed",
				"kind", kind.Kind, "crd", kind.CRD, "state", kind.State, "detail", kind.Detail, "remedy", kind.Remedy)
			c.event(corev1.EventTypeWarning, schemacheck.ReasonUnconfirmed, kind.Sentence())
		}
	}
}

func (c *Checker) event(eventType, reason, message string) {
	if c.Recorder == nil || c.EventOn == nil {
		return
	}
	c.Recorder.Event(c.EventOn, eventType, reason, message)
}
