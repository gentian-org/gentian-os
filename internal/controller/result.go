/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import ctrl "sigs.k8s.io/controller-runtime"

// resultUnlessError returns err alone when it is set and res otherwise.
// controller-runtime ignores the Result of a reconcile that returns an error
// and requeues with its own backoff, so a RequeueAfter beside an error is a
// delay that never happens; scripts/lint/result-with-error refuses the pair.
func resultUnlessError(res ctrl.Result, err error) (ctrl.Result, error) {
	if err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}
