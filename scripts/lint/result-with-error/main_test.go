/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheck(t *testing.T) {
	t.Parallel()

	src := `package p

func refused() (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: 1}, err
}

func forwarded() (reconcile.Result, error) {
	res, err := refused()
	return res, err
}

func literal() (ctrl.Result, error) {
	f := func() (ctrl.Result, error) { return ctrl.Result{Requeue: true}, persist() }
	return f()
}

func accepted() (ctrl.Result, error) {
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 1}, nil
}

func other() (int, error) {
	return 1, err
}
`
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := check(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("findings = %q; want the three in refused, forwarded and the closure in literal", found)
	}
}
