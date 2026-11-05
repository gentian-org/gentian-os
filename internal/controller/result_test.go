/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"errors"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

func TestResultUnlessError(t *testing.T) {
	t.Parallel()

	wait := ctrl.Result{RequeueAfter: time.Minute}
	if res, err := resultUnlessError(wait, nil); err != nil || res != wait {
		t.Fatalf("no error: Result %+v, error %v; want %+v", res, err, wait)
	}
	failed := errors.New("persist failed")
	if res, err := resultUnlessError(wait, failed); !errors.Is(err, failed) || res != (ctrl.Result{}) {
		t.Fatalf("error: Result %+v, error %v; want the error alone", res, err)
	}
}
