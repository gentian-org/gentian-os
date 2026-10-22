/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
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

// zoneName is what a DNS zone in certificates.dnsZones may look like: a
// registrable domain, the pattern the Cluster XRD declares for the item.
var zoneName = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`)

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
	// List marks a setting whose claim value is a list of strings. Over the
	// API it travels as one comma-separated string both ways, the shape the
	// installer reads it in (claim_list_setting); the editor writes it as a
	// block list under its parent.
	List bool
	// Each, on a list setting, is what every item has to match.
	Each *regexp.Regexp
	// InstallStep names the installer step that reads this setting, for the
	// settings the installer applies rather than the operator: a commit
	// changes nothing on the cluster until that step runs again. Empty for
	// the settings Argo CD and the operator pick up on their own.
	InstallStep string
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
	{Path: "certificates.dnsZones", Doc: "Zones external-dns may write besides the kernel domain's: the zones of tenants served on a domain of their own. Comma-separated; the DNS credential has to cover them. Read when the installer installs external-dns (step B-01).", List: true, Each: zoneName, InstallStep: "B-01"},
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
	{Path: "mainAddress.tenant", Doc: "The tenant that may put a website on the cluster's main address, the bare domain, under either tenancy mode. Empty: the user tenant on a single-tenancy cluster, and nobody on a multi-tenancy one, whose main address is then the sign-in form."},
	{Path: "platformRoles.admin", Doc: "The Keycloak group that administers this cluster."},
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
		if items, ok := v.([]any); ok {
			return joinItems(items)
		}
		return scalarString(v)
	}
	return lookupPath(v, path[1:])
}

// joinItems is a list setting as the API carries it: its items joined with
// commas. A list of anything but strings is not a value this catalogue knows.
func joinItems(items []any) (string, bool) {
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return "", false
		}
		out = append(out, s)
	}
	return strings.Join(out, ","), true
}

// splitItems is the API's comma-separated value as the claim's list: trimmed,
// empties dropped, so "a, b" and "a,b" are the same list and "" is the empty one.
func splitItems(v string) []string {
	var out []string
	for _, it := range strings.Split(v, ",") {
		if it = strings.TrimSpace(it); it != "" {
			out = append(out, it)
		}
	}
	return out
}

// validateSetting is what a value is checked against before anything is
// written: that it is a scalar line, that it is one of the values the setting
// accepts, and on a list setting that every item is what the list holds.
func validateSetting(s ClusterSetting, v string) error {
	if strings.ContainsAny(v, "\n\r") {
		return fmt.Errorf("%w: %s", ErrNotScalar, s.Path)
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
			return fmt.Errorf("%w: %s takes one of %s", ErrUnknownSetting, s.Path, strings.Join(s.OneOf, ", "))
		}
	}
	if s.List && s.Each != nil {
		for _, it := range splitItems(v) {
			if !s.Each.MatchString(it) {
				return fmt.Errorf("%w: %s takes zone names such as example.org, not %q", ErrUnknownSetting, s.Path, it)
			}
		}
	}
	return nil
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
		if err := validateSetting(s, v); err != nil {
			return Result{}, err
		}
		paths = append(paths, p)
	}
	// multi -> single on a cluster that carries more than a single-tenancy
	// cluster may. Refused here, loudly and before anything is written: the
	// operator would refuse every such tenant on its next pass, and each would
	// then stand where it was -- its data kept, its hosts still answering,
	// and nothing reconciling it -- with the reason on a status condition.
	// Nothing is removed by either refusal. single -> multi is always fine.
	if gentianov1alpha1.NormalizeTenancyMode(values["tenancyMode"]) == gentianov1alpha1.TenancyModeSingle && values["tenancyMode"] != "" {
		names, err := g.Tenants(ctx)
		if err != nil {
			return Result{}, err
		}
		var others []string
		for _, n := range names {
			if n != platformTenant && n != gentianov1alpha1.SingleUserTenantName {
				others = append(others, n)
			}
		}
		if len(others) > 0 {
			return Result{}, fmt.Errorf(
				"%w: this cluster carries %s, and a single-tenancy cluster admits only the platform tenant "+
					"and one user tenant named %q. Nothing was changed. Retire %s first, or keep tenancyMode multi",
				ErrSingleRefused, strings.Join(others, ", "),
				gentianov1alpha1.SingleUserTenantName, strings.Join(others, ", "))
		}
	}
	sort.Strings(paths)
	message := fmt.Sprintf("feat(cluster): set %s (via %s)", strings.Join(paths, ", "), meta.actor())

	return g.applyClaim(ctx, message, meta, func(text string) (string, string, bool, error) {
		out := text
		changed := false
		for _, p := range paths {
			var next string
			var did bool
			var err error
			if s, _ := settingByPath(p); s.List {
				next, did, err = setClaimList(out, p, splitItems(values[p]))
			} else {
				next, did, err = setClaimValue(out, p, values[p])
			}
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
	for started, attempt := time.Now(), 1; ; attempt++ {
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
		if err := retryPush(ctx, started, attempt); err != nil {
			return Result{}, err
		}
	}
}

// setClaimValue writes one dotted path into the claim text.
//
// Three cases, in order: the key is there and live, so its value is replaced;
// the key is there commented out, which is how the claim documents a setting
// nobody has chosen yet, so the comment becomes the setting; the key is not
// there at all, so it is inserted under its parent, creating the parents it
// needs. Everything else in the file is untouched, byte for byte.
func setClaimValue(text, path, value string) (string, bool, error) {
	return setClaimLeaf(text, path, scalarLeaf(value))
}

// setClaimList writes one dotted path as a block list: the key line and one
// "- item" line per item under it, an empty list as `key: []`. Where the key
// holds a list already, the whole list is replaced; everything else in the
// file is untouched, as for a scalar.
func setClaimList(text, path string, items []string) (string, bool, error) {
	return setClaimLeaf(text, path, listLeaf(items))
}

func setClaimLeaf(text, path string, l leaf) (string, bool, error) {
	newText, changed, err := setKeyPath(text, "spec", strings.Split(path, "."), l)
	if errors.Is(err, errNoRoot) {
		return text, false, fmt.Errorf("the claim has no spec block")
	}
	if errors.Is(err, errNoPath) {
		return text, false, fmt.Errorf("%w: %s", ErrUnknownSetting, path)
	}
	return newText, changed, err
}

// leaf is what the walk writes once it has found, revived or made the place
// for a key: the lines the value takes, how far the value already there
// reaches, and whether it is the same already.
type leaf interface {
	// render is the key with its value, at this indent.
	render(indent int, key string) []string
	// extent is how many lines the value at `at` occupies, the key line included.
	extent(lines []string, at, indent int) int
	// same reports whether the value at `at` is the one to write.
	same(lines []string, at, indent int) bool
}

type scalarLeaf string

func (v scalarLeaf) render(indent int, key string) []string {
	return []string{fmt.Sprintf("%s%s: %s", strings.Repeat(" ", indent), key, quoteScalar(string(v)))}
}
func (scalarLeaf) extent([]string, int, int) int { return 1 }
func (v scalarLeaf) same(lines []string, at, _ int) bool {
	return scalarOf(lines[at]) == string(v)
}

type listLeaf []string

func (items listLeaf) render(indent int, key string) []string {
	pad := strings.Repeat(" ", indent)
	if len(items) == 0 {
		return []string{pad + key + ": []"}
	}
	out := []string{pad + key + ":"}
	for _, it := range items {
		out = append(out, fmt.Sprintf("%s  - %s", pad, quoteScalar(it)))
	}
	return out
}

// extent: the key line, then every following line indented deeper than the
// key -- the items, and any comment or blank line among them, which goes
// with the list it sits in.
func (listLeaf) extent(lines []string, at, indent int) int {
	n := 1
	for i := at + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			// A blank line ends the list unless an item follows it.
			if j := i + 1; j < len(lines) && indentOf(lines[j]) > indent && strings.TrimSpace(lines[j]) != "" {
				n++
				continue
			}
			break
		}
		if indentOf(lines[i]) <= indent {
			break
		}
		n++
	}
	return n
}

func (items listLeaf) same(lines []string, at, indent int) bool {
	cur := itemsOf(lines, at, items.extent(lines, at, indent))
	if len(cur) != len(items) {
		return false
	}
	for i := range cur {
		if cur[i] != items[i] {
			return false
		}
	}
	return true
}

// itemsOf reads the list at `at`: the block form, one "- item" line each, or
// the flow form on the key line, `[a, b]`.
func itemsOf(lines []string, at, extent int) []string {
	var out []string
	if v := scalarOf(lines[at]); strings.HasPrefix(v, "[") {
		for _, it := range strings.Split(strings.Trim(v, "[]"), ",") {
			if it = strings.Trim(strings.TrimSpace(it), `"'`); it != "" {
				out = append(out, it)
			}
		}
		return out
	}
	for i := at + 1; i < at+extent && i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "- ") {
			continue
		}
		it := strings.TrimSpace(strings.TrimPrefix(t, "- "))
		if k := strings.Index(it, " #"); k >= 0 {
			it = strings.TrimSpace(it[:k])
		}
		out = append(out, strings.Trim(it, `"'`))
	}
	return out
}

var (
	errNoRoot = errors.New("no such top-level block")
	errNoPath = errors.New("path not reached")
)

// setKeyPath is setClaimValue under any top-level block, with the keys given
// as a list so one of them may itself contain a dot -- an annotation key
// such as gentianos.io/purge-requested.
func setKeyPath(text, root string, keys []string, l leaf) (string, bool, error) {
	lines := strings.Split(text, "\n")

	specAt := -1
	for i, l := range lines {
		if strings.TrimRight(l, " ") == root+":" && indentOf(l) == 0 {
			specAt = i
			break
		}
	}
	if specAt < 0 {
		return text, false, errNoRoot
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
					return strings.Join(replaceLines(lines, c, 1, l.render(indent, key)), "\n"), true, nil
				}
				// A commented parent cannot hold children; make it real.
				lines[c] = fmt.Sprintf("%s%s:", strings.Repeat(" ", indent), key)
				at = c
			} else {
				// At the end of the file, before its trailing newline -- the
				// empty last element of the split -- so the file keeps ending
				// in one and the key is not written after it.
				pos := end
				for pos == len(lines) && pos > 0 && strings.TrimSpace(lines[pos-1]) == "" {
					pos--
				}
				if last {
					return strings.Join(replaceLines(lines, pos, 0, l.render(indent, key)), "\n"), true, nil
				}
				lines = replaceLines(lines, pos, 0, []string{strings.Repeat(" ", indent) + key + ":"})
				at = pos
			}
		}
		if last {
			if l.same(lines, at, indent) {
				return text, false, nil
			}
			lines = replaceLines(lines, at, l.extent(lines, at, indent), l.render(indent, key))
			return strings.Join(lines, "\n"), true, nil
		}
		start, end = at+1, blockEnd(lines, at+1, indent)
		indent += 2
	}
	return text, false, errNoPath
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

// replaceLines puts `with` in place of the n lines at `at`; n of zero inserts.
func replaceLines(lines []string, at, n int, with []string) []string {
	out := make([]string, 0, len(lines)-n+len(with))
	out = append(out, lines[:at]...)
	out = append(out, with...)
	return append(out, lines[at+n:]...)
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
	// The words a YAML 1.1 reader, Argo CD's among them, takes for a boolean:
	// bare, "off" reaches the cluster as false and a string setting is lost.
	switch strings.ToLower(v) {
	case "y", "n", "yes", "no", "on", "off":
		return strconv.Quote(v)
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return v
	}
	if strings.ContainsAny(v, ":#{}[]&*!|>%@`\"'") || strings.TrimSpace(v) != v {
		return strconv.Quote(v)
	}
	return v
}
