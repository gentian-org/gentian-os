/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package layout

import "testing"

// There is one layout, so a process whose chart forgot the environment still
// answers with this cluster's namespaces rather than with another cluster's.
func TestAProcessWithoutTheEnvironmentRunsThisLayout(t *testing.T) {
	for _, fn := range []Function{Edge, Authentication, Control, GitOps, Secrets, Provisioning, Admission} {
		if got, want := Namespace(fn), Kernel(fn); got != want {
			t.Errorf("%s: %q, want %q", fn, got, want)
		}
	}
}

func TestTheEnvironmentSaysWhereAFunctionRuns(t *testing.T) {
	t.Setenv("GENTIAN_NS_EDGE", "kernel-edge")
	t.Setenv("GENTIAN_NS_AUTHENTICATION", "kernel-authentication")
	if got := Namespace(Edge); got != "kernel-edge" {
		t.Errorf("edge = %q", got)
	}
	if got := Namespace(Authentication); got != "kernel-authentication" {
		t.Errorf("authentication = %q", got)
	}
	if got := Namespace(GitOps); got != Kernel(GitOps) {
		t.Errorf("an unset function falls back to this layout, got %q", got)
	}
}

// What the chart sets and what a process reads are the same names, or the
// layout reaches the operator as silence.
func TestEnvNamesEveryKernelFunctionOfThisLayout(t *testing.T) {
	env := Env()
	if len(env) != len(KernelNamespaces()) {
		t.Fatalf("%d variables for %d namespaces", len(env), len(KernelNamespaces()))
	}
	for _, k := range kernel {
		name, ok := env[EnvPrefix+envSuffix(k.fn)]
		if !ok || name != k.name {
			t.Errorf("%s: env has %q", k.fn, name)
		}
		t.Setenv(EnvPrefix+envSuffix(k.fn), name)
		if Namespace(k.fn) != k.name {
			t.Errorf("%s: set from Env(), Namespace returns %q", k.fn, Namespace(k.fn))
		}
	}
}
