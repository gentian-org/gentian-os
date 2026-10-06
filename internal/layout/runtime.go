/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package layout

import "os"

// EnvPrefix is how a process is told where a kernel function runs:
// GENTIAN_NS_EDGE, GENTIAN_NS_AUTHENTICATION, and so on. The chart sets them
// from kernel/namespaces.yaml.
const EnvPrefix = "GENTIAN_NS_"

// Namespace returns where a kernel function runs.
//
// It is read from the environment every call rather than cached: a reconciler
// asks for this on the path, and a value cached at init is a value a test
// cannot change.
//
// Falling back to this package's own table is the whole of the change that
// removed the second layout. There used to be a map here of where release 4.1
// put each function, because a process started without the environment was
// running on a cluster built that way. There is one layout now, so a missing
// variable is a chart that did not set it -- and answering with the layout
// this binary was built for is right, where answering with another cluster's
// namespaces would have sent the reconciler somewhere that does not exist.
func Namespace(fn Function) string {
	if v := os.Getenv(EnvPrefix + envSuffix(fn)); v != "" {
		return v
	}
	return Kernel(fn)
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
