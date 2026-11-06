/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var updateConditions = flag.Bool("update-conditions", false, "rewrite the recorded condition transcript")

// conditionSetter sets one condition on a fresh object holding conds at
// generation 2, and returns the object's conditions after.
type conditionSetter func(t *testing.T, conds []metav1.Condition, typ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition

// conditionsOld is the transition time of every condition an object starts
// with: whatever a setter writes instead is now.
var conditionsOld = metav1.NewTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))

func describeConditions(conds []metav1.Condition) string {
	var out []string
	for _, c := range conds {
		when := "now"
		switch {
		case c.LastTransitionTime.IsZero():
			when = "zero"
		case c.LastTransitionTime.Equal(&conditionsOld):
			when = "kept"
		}
		out = append(out, fmt.Sprintf("{%s %s %s %q transition=%s generation=%d}", c.Type, c.Status, c.Reason, c.Message, when, c.ObservedGeneration))
	}
	return strings.Join(out, " ")
}

// What each condition setter writes, held to
// testdata/conditions.txt: a new condition, the same one again, and one
// whose reason, message or status changes, beside a condition of another
// type that is left alone.
func TestConditionSettersTranscript(t *testing.T) {
	setters := []struct {
		name string
		set  conditionSetter
	}{
		{"schedule", func(_ *testing.T, conds []metav1.Condition, _ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			o := &gentianov1alpha1.TenantExportSchedule{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
			o.Status.Conditions = conds
			setScheduleCondition(o, st, reason, message)
			return o.Status.Conditions
		}},
		{"export", func(_ *testing.T, conds []metav1.Condition, typ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			o := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
			o.Status.Conditions = conds
			setExportCondition(o, typ, st, reason, message)
			return o.Status.Conditions
		}},
		{"restore", func(_ *testing.T, conds []metav1.Condition, typ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			o := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
			o.Status.Conditions = conds
			setRestoreCondition(o, typ, st, reason, message)
			return o.Status.Conditions
		}},
		{"customization record", func(_ *testing.T, conds []metav1.Condition, typ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			s := &gentianov1alpha1.CustomizationStatus{Conditions: conds}
			setRecordCondition(s, typ, st, reason, message)
			return s.Conditions
		}},
		{"tenant", func(_ *testing.T, conds []metav1.Condition, typ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			o := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
			o.Status.Conditions = conds
			(&TenantReconciler{}).setCondition(o, typ, st, reason, message)
			return o.Status.Conditions
		}},
		{"backup policy", func(_ *testing.T, conds []metav1.Condition, _ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			o := &gentianov1alpha1.BackupPolicy{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
			o.Status.Conditions = conds
			setPolicyCondition(o, st, reason, message)
			return o.Status.Conditions
		}},
		{"app grant", func(_ *testing.T, conds []metav1.Condition, typ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			o := &gentianov1alpha1.AppGrant{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
			o.Status.Conditions = conds
			setAppGrantCondition(o, typ, st, reason, message)
			return o.Status.Conditions
		}},
		{"platform security policy", func(_ *testing.T, conds []metav1.Condition, typ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			o := &gentianov1alpha1.PlatformSecurityPolicy{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
			o.Status.Conditions = conds
			setPSPCondition(o, typ, st, reason, message)
			return o.Status.Conditions
		}},
		{"component", func(t *testing.T, conds []metav1.Condition, _ string, st metav1.ConditionStatus, reason, message string) []metav1.Condition {
			o := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "n", Generation: 2}}
			o.Status.Conditions = conds
			scheme := runtime.NewScheme()
			_ = gentianov1alpha1.AddToScheme(scheme)
			r := &ComponentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(o.DeepCopy()).WithStatusSubresource(o).Build()}
			live := &gentianov1alpha1.Component{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(o), live); err != nil {
				t.Fatal(err)
			}
			o.ResourceVersion = live.ResourceVersion
			if _, err := r.status(context.Background(), o, st, reason, message, 0); err != nil {
				t.Fatal(err)
			}
			return o.Status.Conditions
		}},
	}
	other := metav1.Condition{Type: "Other", Status: metav1.ConditionTrue, Reason: "R", Message: "m", LastTransitionTime: conditionsOld, ObservedGeneration: 1}
	steps := []struct {
		name        string
		start       bool
		st          metav1.ConditionStatus
		reason, msg string
	}{
		{"new", false, metav1.ConditionTrue, "A", "one"},
		{"the same", true, metav1.ConditionTrue, "A", "one"},
		{"another reason", true, metav1.ConditionTrue, "B", "one"},
		{"another message", true, metav1.ConditionTrue, "A", "two"},
		{"another status", true, metav1.ConditionFalse, "A", "one"},
	}
	var out []string
	for _, s := range setters {
		out = append(out, "# "+s.name)
		for _, step := range steps {
			// Each setter's own type: those that take one are given Ready.
			conds := []metav1.Condition{other}
			if step.start {
				conds = append(conds, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "A", Message: "one",
					LastTransitionTime: conditionsOld, ObservedGeneration: 1})
				if s.name == "backup policy" {
					conds[1].Type = conditionPolicyAccepted
				}
			}
			got := s.set(t, conds, "Ready", step.st, step.reason, step.msg)
			out = append(out, step.name+": "+describeConditions(got))
		}
	}
	path := filepath.Join("testdata", "conditions.txt")
	got := strings.Join(out, "\n") + "\n"
	if *updateConditions {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // A test fixture.
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // A test fixture.
	if err != nil {
		t.Fatalf("%v (run with -update-conditions to record)", err)
	}
	if string(want) != got {
		t.Fatalf("the transcript differs from %s (run with -update-conditions to record)\n--- got\n%s--- want\n%s", path, got, want)
	}
}
