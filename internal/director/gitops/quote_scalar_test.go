/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"fmt"
	"strconv"
	"testing"

	yamlv2 "go.yaml.in/yaml/v2"
	yamlv3 "go.yaml.in/yaml/v3"
)

// yaml11Spellings is every implicit type of YAML 1.1 (https://yaml.org/type/)
// in the spellings its resolvers accept. Each is a string a setting may hold
// and a bare scalar a reader takes for something else.
var yaml11Spellings = map[string][]string{
	"bool": {
		"y", "Y", "yes", "Yes", "YES", "n", "N", "no", "No", "NO",
		"True", "TRUE", "False", "FALSE",
		"on", "On", "ON", "off", "Off", "OFF",
	},
	"null": {"~", "null", "Null", "NULL"},
	"int": {
		"0b1010", "-0b1010", "0b1_0", // binary
		"012", "0777", "-017", "0_7", // octal
		"+12", "1_000", "-0", "00", // decimal, not canonical
		"0x10", "0xFF", "-0x1A", "0x_1", // hex
		"190:20:30", "1:20", "-3:25:45", // base 60
		"99999999999999999999", // past int64: read as a float
	},
	"float": {
		"1.0", "1.5", "-1.5", "+1.5", "0.5", ".5", "1.", "6.8523015e+5",
		"685.230_15e+03", "685_230.15", "1e3", "1E-3", // exponent
		"190:20:30.15",                           // base 60
		".inf", ".Inf", ".INF", "-.inf", "+.inf", // infinity
		".nan", ".NaN", ".NAN", // not a number
	},
	"timestamp": {
		"2001-12-14", "2002-1-1", "2001-12-14t21:59:43.10-05:00",
		"2001-12-14 21:59:43.10 -5", "2001-12-15T02:59:43.1Z", "2001-12-15 2:59:43.10",
	},
	"merge": {"<<"},
	"value": {"="},
}

// Strings a setting may hold that are not a type and must stay readable.
var plainStrings = []string{
	"external", "offline", "staging", "500m", "512Mi", "1Gi", "v1.2.3",
	"mail.example.org", "https", "yes-please", "nothing", "Truee", "0x", "1-2",
	"docs.example.org CNAME example.github.io",
}

// A value written by quoteScalar reads back as the same text under a YAML 1.1
// reader and a YAML 1.2 one. The values a claim writes bare on purpose,
// booleans and canonical integers, read back as that type with the same text.
func TestQuoteScalarRoundTripsUnderYAML11And12(t *testing.T) {
	t.Parallel()
	var all []string
	for kind, spellings := range yaml11Spellings {
		for _, v := range spellings {
			if got := quoteScalar(v); got == v {
				t.Errorf("YAML 1.1 %s %q is left bare", kind, v)
			}
			all = append(all, v)
		}
	}
	all = append(all, plainStrings...)
	all = append(all, "", " padded", "trailing ", "a: b", "# note", "- item", "? key", ",x",
		"gentian:platform:admin", "ops@example.org", `say "hi"`, `back\slash`, "tab\there", "bell\a")
	for _, v := range all {
		roundTrip(t, v, quoteScalar(v), true)
	}

	for _, v := range plainStrings {
		if got := quoteScalar(v); got != v {
			t.Errorf("plain string %q is quoted: %s", v, got)
		}
	}

	for _, v := range []string{"true", "false", "0", "587", "-1", "9223372036854775807"} {
		if got := quoteScalar(v); got != v {
			t.Errorf("%q is quoted, and a setting of that type would become a string: %s", v, got)
		}
		roundTrip(t, v, v, false)
	}
}

// roundTrip reads "k: <written>" with both readers and compares what comes
// back with v: as a string when asString, else as the text of the typed value.
func roundTrip(t *testing.T, v, written string, asString bool) {
	t.Helper()
	doc := "k: " + written + "\n"
	var v11 map[string]any
	if err := yamlv2.Unmarshal([]byte(doc), &v11); err != nil {
		t.Errorf("YAML 1.1 cannot read %q: %v", doc, err)
		return
	}
	var v12 map[string]any
	if err := yamlv3.Unmarshal([]byte(doc), &v12); err != nil {
		t.Errorf("YAML 1.2 cannot read %q: %v", doc, err)
		return
	}
	for reader, got := range map[string]any{"YAML 1.1": v11["k"], "YAML 1.2": v12["k"]} {
		if asString {
			if s, ok := got.(string); !ok || s != v {
				t.Errorf("%s reads %q, written as %s, back as %#v", reader, v, written, got)
			}
			continue
		}
		if _, isString := got.(string); isString || typedText(got) != v {
			t.Errorf("%s reads %q back as %#v, not as that type", reader, v, got)
		}
	}
}

func typedText(v any) string {
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	}
	return fmt.Sprintf("%v", v)
}
