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

import "os"

// EnvPrefix is how a process is told where a kernel function runs:
// GENTIAN_NS_EDGE, GENTIAN_NS_AUTHENTICATION, and so on. The chart sets them
// from kernel/namespaces.yaml.
const EnvPrefix = "GENTIAN_NS_"

// v4 is the layout release 4.1 installs. A process started without the
// environment above runs it — which is the case for every cluster that has not
// been rebuilt, so the fallback has to be exactly what those clusters have.
var v4 = map[Function]string{
	GitOps:         "argocd",
	Provisioning:   "crossplane-system",
	Secrets:        "openbao",
	Seal:           "openbao",
	Data:           "platform-kernel",
	Authentication: "platform-kernel",
	Authorization:  "platform-kernel",
	Control:        "gentian-system",
	Edge:           "platform-kernel",
	Admission:      "kyverno",
	Observability:  "",
}

// Namespace returns where a kernel function runs.
//
// It is read from the environment every call rather than cached: a reconciler
// asks for this on the path, and a value cached at init is a value a test
// cannot change.
func Namespace(fn Function) string {
	if v := os.Getenv(EnvPrefix + envSuffix(fn)); v != "" {
		return v
	}
	return v4[fn]
}

// Env returns the variables that tell a process this layout, for a chart or a
// container spec to set.
func Env() map[string]string {
	out := make(map[string]string, len(kernel))
	for _, k := range kernel {
		out[EnvPrefix+envSuffix(k.fn)] = k.name
	}
	return out
}

func envSuffix(fn Function) string {
	b := []byte(fn)
	for i := range b {
		if b[i] == '-' {
			b[i] = '_'
		} else if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
