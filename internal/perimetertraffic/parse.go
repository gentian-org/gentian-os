/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package perimetertraffic records what each tenant's public entries carry:
// requests per entry and per declared path, by response class, from the
// counters the perimeter's Envoy keeps anyway.
//
// Envoy counts every request it hands to a backend, per route rule, whether
// or not anybody reads the count. The perimeter route of an entry has one
// rule per declared path, so its counters are already the numbers a
// perimeter approver asks for: which entries, and which paths of them, carry
// the most, and how much of it is refused. The sampler reads them a few
// times an hour and keeps the differences in each tenant's own database,
// beside its resource usage; the publishing proxies in the DMZ are left as
// bare as they are.
package perimetertraffic

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// Counts is what Envoy counted for one route rule.
type Counts struct {
	Requests int64 `json:"requests"`
	C2xx     int64 `json:"2xx"`
	C3xx     int64 `json:"3xx"`
	C4xx     int64 `json:"4xx"`
	C5xx     int64 `json:"5xx"`
}

func (c Counts) zero() bool { return c == Counts{} }

// minus is the traffic between two readings of one Envoy. A counter that
// went down was reset -- the process restarted -- and what it reads now is
// all it has counted since.
func (c Counts) minus(before Counts) Counts {
	d := func(now, then int64) int64 {
		if now < then {
			return now
		}
		return now - then
	}
	return Counts{
		Requests: d(c.Requests, before.Requests),
		C2xx:     d(c.C2xx, before.C2xx),
		C3xx:     d(c.C3xx, before.C3xx),
		C4xx:     d(c.C4xx, before.C4xx),
		C5xx:     d(c.C5xx, before.C5xx),
	}
}

func (c *Counts) add(o Counts) {
	c.Requests += o.Requests
	c.C2xx += o.C2xx
	c.C3xx += o.C3xx
	c.C4xx += o.C4xx
	c.C5xx += o.C5xx
}

// Rule names one route rule as Envoy names its cluster:
// httproute/<namespace>/<route>/rule/<n>.
type Rule struct {
	Namespace string
	Route     string
	Index     int
}

func ruleOf(cluster string) (Rule, bool) {
	parts := strings.Split(cluster, "/")
	if len(parts) != 5 || parts[0] != "httproute" || parts[3] != "rule" {
		return Rule{}, false
	}
	n, err := strconv.Atoi(parts[4])
	if err != nil {
		return Rule{}, false
	}
	return Rule{Namespace: parts[1], Route: parts[2], Index: n}, true
}

// Parse reads Envoy's Prometheus exposition and keeps the request counters
// of the route rules in tenants' DMZs. Everything else is passed over.
func Parse(body []byte) map[Rule]Counts {
	out := map[Rule]Counts{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		var metric string
		switch {
		case strings.HasPrefix(line, "envoy_cluster_upstream_rq_total{"):
			metric = "total"
		case strings.HasPrefix(line, "envoy_cluster_upstream_rq_xx{"):
			metric = "xx"
		default:
			continue
		}
		open, close := strings.IndexByte(line, '{'), strings.LastIndexByte(line, '}')
		if open < 0 || close < open {
			continue
		}
		labels := parseLabels(line[open+1 : close])
		value, err := strconv.ParseFloat(strings.TrimSpace(line[close+1:]), 64)
		if err != nil {
			continue
		}
		rule, ok := ruleOf(labels["envoy_cluster_name"])
		if !ok || !strings.HasSuffix(rule.Namespace, "-dmz") {
			continue
		}
		c := out[rule]
		n := int64(value)
		if metric == "total" {
			c.Requests = n
		} else {
			switch labels["envoy_response_code_class"] {
			case "2":
				c.C2xx = n
			case "3":
				c.C3xx = n
			case "4":
				c.C4xx = n
			case "5":
				c.C5xx = n
			}
		}
		out[rule] = c
	}
	return out
}

// parseLabels reads `a="x",b="y"`. Envoy's label values here are cluster
// names and digits: no escaped quote occurs, and one that did would end the
// value early rather than be misread as another label.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 || eq+1 >= len(s) || s[eq+1] != '"' {
			break
		}
		key := strings.TrimSpace(s[:eq])
		rest := s[eq+2:]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			break
		}
		out[key] = rest[:end]
		s = strings.TrimPrefix(rest[end+1:], ",")
	}
	return out
}
