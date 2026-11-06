/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setStatusCondition sets the condition of type typ in conds, as the API
// conventions have it (apimeta.SetStatusCondition): its transition time
// moves only when its status does, and it records the generation it was
// decided for.
func setStatusCondition(conds *[]metav1.Condition, generation int64, typ string, st metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(conds, metav1.Condition{
		Type: typ, Status: st, Reason: reason, Message: message, ObservedGeneration: generation,
	})
}

// replaceChangedCondition sets the condition of type typ in conds only when
// its status, reason or message changes, and then whole: with now as its
// transition time and generation as its generation. One that says the same
// is left as it is.
func replaceChangedCondition(conds *[]metav1.Condition, generation int64, typ string, st metav1.ConditionStatus, reason, message string) {
	cond := metav1.Condition{
		Type: typ, Status: st, Reason: reason, Message: message,
		LastTransitionTime: metav1.Now(), ObservedGeneration: generation,
	}
	if c := apimeta.FindStatusCondition(*conds, typ); c != nil {
		if c.Status != st || c.Reason != reason || c.Message != message {
			*c = cond
		}
		return
	}
	*conds = append(*conds, cond)
}

// stampCondition sets the condition of type typ in conds with now as its
// transition time, whether or not anything about it changed. Its generation
// is left as it was.
func stampCondition(conds *[]metav1.Condition, typ string, st metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	if c := apimeta.FindStatusCondition(*conds, typ); c != nil {
		c.Status, c.Reason, c.Message, c.LastTransitionTime = st, reason, message, now
		return
	}
	*conds = append(*conds, metav1.Condition{Type: typ, Status: st, Reason: reason, Message: message, LastTransitionTime: now})
}
