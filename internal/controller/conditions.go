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
