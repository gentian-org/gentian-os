/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/package layout

import "testing"

func TestAProcessWithoutTheEnvironmentRunsTheV4Layout(t *testing.T) {
	for fn, want := range map[Function]string{
		Edge: "platform-kernel", Authentication: "platform-kernel", Control: "gentian-system",
		GitOps: "argocd", Secrets: "openbao", Provisioning: "crossplane-system", Admission: "kyverno",
	} {
		if got := Namespace(fn); got != want {
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
	if got := Namespace(GitOps); got != "argocd" {
		t.Errorf("an unset function keeps its v4 name, got %q", got)
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
