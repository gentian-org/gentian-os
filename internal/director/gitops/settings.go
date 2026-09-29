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
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// The cluster's settings live in one file, the Cluster claim in git, and this
// is how they are read and changed.
//
// A line editor rather than a YAML round-trip, for the reason the addons
// editor gives: the claim is hand-maintained and carries seventy-odd lines of
// comment explaining what each setting means and what happens if it is wrong.
// A load and dump cycle would drop every one of them and turn each change into
// a whole-file diff nobody can review.
//
// Only leaves, and only ones the schema knows. A caller may change a value; it
// may not invent a setting, restructure the document, or reach a secret
// reference. What may be set is the allowlist below, which is the part of the
// claim that is policy rather than plumbing.

// ErrUnknownSetting is a path no cluster setting is written at.
var ErrUnknownSetting = errors.New("unknown cluster setting")

// ErrNotScalar is a value that is not a scalar a claim line can hold.
var ErrNotScalar = errors.New("setting takes a scalar value")

// ClusterSetting describes one changeable leaf of the claim.
type ClusterSetting struct {
	// Path is the dotted path under spec, e.g. "mail.serviceMode".
	Path string
	// Doc is one line, so an API answer explains itself without the schema.
	Doc string
	// OneOf, when set, is the values the setting accepts.
	OneOf []string
	// Default is what the Cluster XRD applies when the claim does not carry
	// this setting, empty when the schema declares none. Compiled in from the
	// XRD (scripts/gen/gen-cluster-setting-defaults.py), because a default a
	// console shows must be the one the cluster will actually apply.
	Default string
}

// clusterSettings is every setting the director will write.
//
// Deliberately an allowlist and not the whole schema. The claim also carries
// secret references, the vault's address and the namespaces the installer was
// run with: plumbing that belongs to whoever installed the cluster and must
// not be reachable over an API. What is here is the policy a platform
// administrator changes during the life of a cluster.
var clusterSettings = []ClusterSetting{
	{Path: "certificates.acmeEnv", Doc: "Which Let's Encrypt environment issues certificates.", OneOf: []string{"staging", "production"}},
	{Path: "certificates.issuerMode", Doc: "How control of the domain is proved.", OneOf: []string{"acme-dns01", "acme-http01", "self-signed", "private-ca"}},
	{Path: "certManager.letsencryptEmail", Doc: "Where Let's Encrypt sends expiry warnings."},
	{Path: "llm.enabled", Doc: "Whether this cluster serves language models.", OneOf: []string{"true", "false"}},
	{Path: "llm.gpuAcceleration", Doc: "Whether model serving uses a GPU.", OneOf: []string{"true", "false"}},
	{Path: "mail.serviceMode", Doc: "Whether the platform runs its own mail stack in system-mail, or relays through an external provider.", OneOf: []string{"system", "external"}},
	{Path: "mail.host", Doc: "The external relay's hostname, when mail is external."},
	{Path: "mail.port", Doc: "The external relay's port."},
	{Path: "mail.starttls", Doc: "Whether the relay is reached with STARTTLS.", OneOf: []string{"true", "false"}},
	{Path: "mail.ssl", Doc: "Whether the relay is reached over implicit TLS.", OneOf: []string{"true", "false"}},
	{Path: "mail.egressHost", Doc: "The name that resolves to the address mail leaves from, for SPF."},
	{Path: "tenancyMode", Doc: "Whether this cluster serves one tenant or many.", OneOf: []string{"single", "multi"}},
	{Path: "platformRoles.admin", Doc: "The Keycloak group that administers this cluster."},
	{Path: "platformRoles.auditor", Doc: "The Keycloak group that may read across tenants."},
	{Path: "platformRoles.securityOfficer", Doc: "The Keycloak group that approves what escapes the default posture."},
	{Path: "platformRoles.serviceAdmin", Doc: "The Keycloak group that runs the system services."},
	{Path: "platformRoles.sharedAppsAdmin", Doc: "The Keycloak group that runs shared applications."},
	{Path: "platformRoles.breakGlass", Doc: "The Keycloak group for recovery when the normal path is down."},
	{Path: "tenantDefaults.limitRange.defaultCpu", Doc: "Default CPU limit for a tenant's containers."},
	{Path: "tenantDefaults.limitRange.defaultMemory", Doc: "Default memory limit for a tenant's containers."},
	{Path: "tenantDefaults.limitRange.defaultRequestCpu", Doc: "Default CPU request for a tenant's containers."},
	{Path: "tenantDefaults.limitRange.defaultRequestMemory", Doc: "Default memory request for a tenant's containers."},
}

// ClusterSettings is the catalogue: what may be set, what each means, and
// what applies when it is not set.
func ClusterSettings() []ClusterSetting {
	out := make([]ClusterSetting, len(clusterSettings))
	copy(out, clusterSettings)
	for i := range out {
		out[i].Default = clusterSettingDefaults[out[i].Path]
	}
	return out
}

func settingByPath(path string) (ClusterSetting, bool) {
	for _, s := range clusterSettings {
		if s.Path == path {
			return s, true
		}
	}
	return ClusterSetting{}, false
}

// ClusterSettingValues reads the current value of every settable path.
//
// Absent is absent: a setting the claim does not carry is missing from the
// answer rather than reported as empty, because "unset" and "set to nothing"
// are different and the schema's default applies to only one of them.
func (g *GitOps) ClusterSettingValues(ctx context.Context) (map[string]string, error) {
	var claim map[string]any
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		return nil, err
	}
	spec, _ := claim["spec"].(map[string]any)
	out := map[string]string{}
	for _, s := range clusterSettings {
		if v, ok := lookupPath(spec, strings.Split(s.Path, ".")); ok {
			out[s.Path] = v
		}
	}
	return out, nil
}

func lookupPath(node any, path []string) (string, bool) {
	m, ok := node.(map[string]any)
	if !ok || len(path) == 0 {
		return "", false
	}
	v, present := m[path[0]]
	if !present {
		return "", false
	}
	if len(path) == 1 {
		return scalarString(v)
	}
	return lookupPath(v, path[1:])
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	case nil:
		return "", false
	}
	return "", false
}

// SetClusterSettings changes the named settings in the claim and commits.
//
// All of them in one commit: a change to `mail.serviceMode` that lands without
// the `mail.host` it needs is a cluster that has stopped sending mail between
// two commits, and the reviewer of the second one cannot see why.
func (g *GitOps) SetClusterSettings(ctx context.Context, values map[string]string, meta Meta) (Result, error) {
	if len(values) == 0 {
		return Result{Status: "unchanged"}, nil
	}
	paths := make([]string, 0, len(values))
	for p, v := range values {
		s, known := settingByPath(p)
		if !known {
			return Result{}, fmt.Errorf("%w: %s", ErrUnknownSetting, p)
		}
		if strings.ContainsAny(v, "\n\r") {
			return Result{}, fmt.Errorf("%w: %s", ErrNotScalar, p)
		}
		if len(s.OneOf) > 0 {
			allowed := false
			for _, o := range s.OneOf {
				if o == v {
					allowed = true
					break
				}
			}
			if !allowed {
				return Result{}, fmt.Errorf("%w: %s takes one of %s", ErrUnknownSetting, p, strings.Join(s.OneOf, ", "))
			}
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	message := fmt.Sprintf("feat(cluster): set %s (via %s)", strings.Join(paths, ", "), meta.actor())

	return g.applyClaim(ctx, message, meta, func(text string) (string, string, bool, error) {
		out := text
		changed := false
		for _, p := range paths {
			next, did, err := setClaimValue(out, p, values[p])
			if err != nil {
				return text, "", false, err
			}
			out = next
			changed = changed || did
		}
		if !changed {
			return text, "unchanged", false, nil
		}
		return out, "updated", true, nil
	})
}

// applyClaim is apply for the Cluster claim: the same sync, edit, commit and
// push, against the one file that is not a tenant's.
func (g *GitOps) applyClaim(ctx context.Context, message string, meta Meta, fn edit) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for attempt := 1; attempt <= maxPushAttempts; attempt++ {
		if err := g.ensureRepo(ctx); err != nil {
			return Result{}, err
		}
		cluster := g.cluster
		if cluster == "" {
			cluster = "default-cluster"
		}
		file := filepath.Join(g.path, "clusters", cluster, "kernel", "claims", "cluster.yaml")
		content, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			return Result{}, ErrNoClusterClaim
		}
		if err != nil {
			return Result{}, err
		}
		text, status, changed, err := fn(string(content))
		if err != nil {
			return Result{}, fmt.Errorf("%w in %s", err, file)
		}
		if !changed {
			return Result{Status: status}, nil
		}
		// Parse what is about to be committed. A line editor can produce
		// something that is no longer YAML, and the place to find that out is
		// here rather than in Argo CD an hour later.
		var probe map[string]any
		if err := yaml.Unmarshal([]byte(text), &probe); err != nil {
			return Result{}, fmt.Errorf("the edited claim is not valid YAML: %w", err)
		}
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			return Result{}, err
		}
		err = g.commit(ctx, file, message, meta)
		if err == nil {
			return g.landed(ctx, status)
		}
		if !errors.Is(err, errPushRejected) {
			return Result{}, err
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Duration(rand.Int63n(int64(attempt) * int64(40*time.Millisecond)))):
		}
	}
	return Result{}, ErrPushContended
}

// setClaimValue writes one dotted path into the claim text.
//
// Three cases, in order: the key is there and live, so its value is replaced;
// the key is there commented out, which is how the claim documents a setting
// nobody has chosen yet, so the comment becomes the setting; the key is not
// there at all, so it is inserted under its parent, creating the parents it
// needs. Everything else in the file is untouched, byte for byte.
func setClaimValue(text, path, value string) (string, bool, error) {
	lines := strings.Split(text, "\n")
	keys := strings.Split(path, ".")

	specAt := -1
	for i, l := range lines {
		if strings.TrimRight(l, " ") == "spec:" && indentOf(l) == 0 {
			specAt = i
			break
		}
	}
	if specAt < 0 {
		return text, false, fmt.Errorf("the claim has no spec block")
	}

	// Walk down, one key at a time, inside the extent of the parent found so far.
	start, end, indent := specAt+1, len(lines), 2
	for depth, key := range keys {
		at := findKeyLine(lines, start, end, indent, key)
		last := depth == len(keys)-1
		if at < 0 {
			// Not live. A commented placeholder is still a home for it.
			if c := findCommentedKeyLine(lines, start, end, indent, key); c >= 0 {
				if last {
					lines[c] = fmt.Sprintf("%s%s: %s", strings.Repeat(" ", indent), key, quoteScalar(value))
					return strings.Join(lines, "\n"), true, nil
				}
				// A commented parent cannot hold children; make it real.
				lines[c] = fmt.Sprintf("%s%s:", strings.Repeat(" ", indent), key)
				at = c
			} else {
				inserted := insertKey(lines, end, indent, key, last, value)
				if last {
					return strings.Join(inserted, "\n"), true, nil
				}
				lines = inserted
				at = end
			}
		}
		if last {
			cur := scalarOf(lines[at])
			if cur == value {
				return text, false, nil
			}
			lines[at] = fmt.Sprintf("%s%s: %s", strings.Repeat(" ", indent), key, quoteScalar(value))
			return strings.Join(lines, "\n"), true, nil
		}
		start, end = at+1, blockEnd(lines, at+1, indent)
		indent += 2
	}
	return text, false, fmt.Errorf("%w: %s", ErrUnknownSetting, path)
}

// findKeyLine finds `<indent><key>:` between start and end.
func findKeyLine(lines []string, start, end, indent int, key string) int {
	want := strings.Repeat(" ", indent) + key + ":"
	for i := start; i < end && i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, want) && indentOf(l) == indent {
			return i
		}
	}
	return -1
}

// findCommentedKeyLine finds the claim's own way of writing "not chosen":
// a commented key at the right depth, with or without a value.
func findCommentedKeyLine(lines []string, start, end, indent int, key string) int {
	for i := start; i < end && i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "#") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(t, "#"))
		if strings.HasPrefix(body, key+":") {
			return i
		}
	}
	return -1
}

// blockEnd is the first line after start that leaves the block at indent.
func blockEnd(lines []string, start, indent int) int {
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if indentOf(l) <= indent {
			return i
		}
	}
	return len(lines)
}

// insertKey puts a new key at the end of its block, as a leaf or a parent.
func insertKey(lines []string, at, indent int, key string, leaf bool, value string) []string {
	line := strings.Repeat(" ", indent) + key + ":"
	if leaf {
		line += " " + quoteScalar(value)
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:at]...)
	out = append(out, line)
	return append(out, lines[at:]...)
}

func scalarOf(line string) string {
	_, after, ok := strings.Cut(line, ":")
	if !ok {
		return ""
	}
	v := strings.TrimSpace(after)
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return strings.Trim(v, `"'`)
}

// quoteScalar quotes only what YAML would otherwise read as something else.
// An unquoted value keeps the file looking like the one a person wrote.
func quoteScalar(v string) string {
	if v == "" {
		return `""`
	}
	if v == "true" || v == "false" || v == "null" || v == "~" {
		return v
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return v
	}
	if strings.ContainsAny(v, ":#{}[]&*!|>%@`\"'") || strings.TrimSpace(v) != v {
		return strconv.Quote(v)
	}
	return v
}
