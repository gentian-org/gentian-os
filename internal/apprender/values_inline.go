/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/gentian-org/gentian-os/internal/kernel/trustanchor"
)

// The release's inline values: the profile's extraValues with the tenant's
// overrides, an addon's overlay and activation, and what the platform adds
// to every app (its pod labels, pull Secrets and the cluster's trust
// anchor). Merged the way the Composition merged them, with sprig's
// mergeOverwrite (valuesMergeOverwrite), so a release it installed is told
// the same values and Helm makes no new revision.

// valuesAnchorDir is where the cluster's trust anchor is mounted.
const valuesAnchorDir = "/opt/gentian-trust-anchor"

// inlineValues renders the release's values.
func (r *valuesRender) inlineValues() (map[string]interface{}, error) {
	in := r.in
	vals := map[string]interface{}{}

	// Only facts about the app's own context are substituted -- its tenant,
	// namespace and domain -- never an endpoint: one names nothing to
	// connect to and carries nothing to authenticate with.
	extra, err := valuesRaw(r.spec.Package.ExtraValues)
	if err != nil {
		return nil, fmt.Errorf("extraValues of %s: %w", in.ProfileName, err)
	}
	if len(extra) > 0 {
		raw, err := json.Marshal(extra)
		if err != nil {
			return nil, err
		}
		s := r.substitute(string(raw))
		if kd := in.KernelServices["KERNEL_DOMAIN"]; kd != "" {
			s = strings.ReplaceAll(s, "${KERNEL_DOMAIN}", kd)
		}
		if ip := in.ClusterConfig["node.ip"]; ip != "" {
			s = strings.ReplaceAll(s, "${NODE_IP}", ip)
		}
		if err := json.Unmarshal([]byte(s), &vals); err != nil {
			return nil, fmt.Errorf("extraValues of %s after substitution: %w", in.ProfileName, err)
		}
	}
	if len(in.ExtraValues) > 0 {
		valuesMergeOverwrite(vals, valuesCopy(in.ExtraValues))
	}
	if in.Replicas != nil && *in.Replicas != 0 {
		valuesMergeOverwrite(vals, map[string]interface{}{"replicaCount": *in.Replicas})
	}

	if err := r.addonValues(vals); err != nil {
		return nil, err
	}

	// A chart the platform places is the platform's own, and is told only
	// what it declares it takes: its values are held to a schema, and the
	// conventions below are a catalog chart's. Its pods are selected by
	// their release, and its pull Secrets named only when the tenant
	// declared a repository.
	if in.Placed {
		if len(in.PullSecrets.Images) > 0 {
			r.pullSecretValues(vals)
		}
	} else {
		// The tenant's kernel-access NetworkPolicy selects the app's pods
		// by these.
		podLabels, _ := vals["podLabels"].(map[string]interface{})
		if podLabels == nil {
			podLabels = map[string]interface{}{}
		}
		valuesMergeOverwrite(podLabels, map[string]interface{}{"gentianos.io/app": in.App, "gentianos.io/tenant": r.tenant})
		valuesMergeOverwrite(vals, map[string]interface{}{"podLabels": podLabels})

		r.pullSecretValues(vals)
		r.trustAnchorValues(vals)
	}

	// Canonical JSON types, as the Composition's toJson | fromJson left
	// them, so the values hash the same from one reconcile to the next.
	raw, err := json.Marshal(vals)
	if err != nil {
		return nil, err
	}
	canonical := map[string]interface{}{}
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

// substitute expands the app's identity placeholders.
func (r *valuesRender) substitute(s string) string {
	for _, sub := range [][2]string{
		{"${TENANT_DOMAIN}", r.in.TenantDomain()},
		{"${TENANT_NAMESPACE}", r.ns},
		{"${TENANT_ID}", r.tenant},
		{"${APP_ID}", r.in.App},
	} {
		s = strings.ReplaceAll(s, sub[0], sub[1])
	}
	return s
}

// addonValues overlays what a selected addon needs from the base's chart
// and renders the base's activation recipe for the selection.
func (r *valuesRender) addonValues(vals map[string]interface{}) error {
	c := r.spec.Customization
	if c == nil {
		return nil
	}
	selected := map[string]bool{}
	for _, a := range r.in.Addons {
		selected[a] = true
	}
	for _, overlay := range c.AddonValues {
		values, err := valuesRaw(overlay.Values)
		if err != nil {
			return fmt.Errorf("addonValues of %s: %w", r.in.ProfileName, err)
		}
		if overlay.WhenAddon != "" && len(values) > 0 && selected[overlay.WhenAddon] {
			valuesMergeOverwrite(vals, values)
		}
	}

	// Rendered whenever the base declares a recipe, an empty selection too:
	// the script reconciles, so an empty selection is how deselecting
	// everything is said.
	act := c.AddonActivation
	if act == nil || act.ValuesPath == "" || act.Script == "" {
		return nil
	}
	script := strings.ReplaceAll(act.Script, "__GENTIAN_ADDON_IDS__", strings.Join(r.in.Addons, " "))
	// The script never goes through the extraValues pass, and is as
	// app-specific as an extraValues hook, so it gets the same placeholders.
	script = r.substitute(script)
	parts := strings.Split(act.ValuesPath, ".")
	// Appended to what the app already runs at this path: a chart's single
	// lifecycle hook usually carries the app's own setup, which replacing it
	// would delete.
	if existing, ok := valuesAt(vals, parts).(string); ok && existing != "" {
		script = existing + "\n\n# --- appended by Gentian: addon activation ---\n" + script
	}
	valuesMergeOverwrite(vals, valuesNest(parts, script))
	return nil
}

// pullSecretValues names the pull Secrets in the two values charts read
// them from: the cluster's own, then the repositories' the tenant declared.
func (r *valuesRender) pullSecretValues(vals map[string]interface{}) {
	var names []string
	has := func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	if list, ok := vals["imagePullSecrets"].([]interface{}); ok {
		for _, entry := range list {
			switch e := entry.(type) {
			case string:
				names = append(names, e)
			case map[string]interface{}:
				if name, _ := e["name"].(string); name != "" {
					names = append(names, name)
				}
			}
		}
	}
	if len(names) == 0 {
		names = []string{"registry-credentials"}
	}
	for _, name := range r.in.PullSecrets.Images {
		if !has(name) {
			names = append(names, name)
		}
	}
	// Bitnami charts take a list of names; a profile whose chart takes the
	// standard list of references says so.
	standard := r.in.ProfileAnnotations["gentianos.io/standard-image-pull-secrets"] == "true"
	list := func() []interface{} {
		out := make([]interface{}, 0, len(names))
		for _, n := range names {
			if standard {
				out = append(out, map[string]interface{}{"name": n})
			} else {
				out = append(out, n)
			}
		}
		return out
	}
	global, _ := vals["global"].(map[string]interface{})
	if global == nil {
		global = map[string]interface{}{}
	}
	valuesMergeOverwrite(global, map[string]interface{}{"imagePullSecrets": list()})
	valuesMergeOverwrite(vals, map[string]interface{}{"imagePullSecrets": list(), "global": global})
}

// trustAnchorValues hands the cluster's trust anchor to the app's
// workloads when a public CA did not sign the kernel's hostnames, or every
// TLS call to the kernel's issuer fails. The operator replicates the anchor
// into each namespace under one fixed name, which is what is mounted.
func (r *valuesRender) trustAnchorValues(vals map[string]interface{}) {
	if r.in.ClusterConfig["certificates.trustAnchorSecret"] == "" && r.in.KernelServices["ACME_STAGING"] != "true" {
		return
	}
	caFile := valuesAnchorDir + "/ca.crt"
	volume := func() map[string]interface{} {
		return map[string]interface{}{
			"name":   "kernel-trust-anchor",
			"secret": map[string]interface{}{"secretName": trustanchor.SecretName},
		}
	}
	mounts := func() []interface{} {
		return []interface{}{map[string]interface{}{"name": "kernel-trust-anchor", "mountPath": valuesAnchorDir, "readOnly": true}}
	}
	var env []interface{}
	environment := map[string]interface{}{}
	for _, name := range []string{"REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "DENO_CERT"} {
		env = append(env, map[string]interface{}{"name": name, "value": caFile})
		environment[name] = caFile
	}

	// A Java app trusts it through a truststore: one that already passes
	// javaOpts, or one with an OIDC client.
	javaOpts, _ := vals["javaOpts"].([]interface{})
	clientID := ""
	if id := r.services().Identity; id != nil && id.OIDC != nil {
		clientID = id.OIDC.ClientID
	}
	needsJavaTrust := len(javaOpts) > 0 || clientID != ""
	if needsJavaTrust {
		for _, opt := range []string{
			"-Djavax.net.ssl.trustStore=" + valuesAnchorDir + "/truststore.jks",
			"-Djavax.net.ssl.trustStoreType=JKS",
			"-Djavax.net.ssl.trustStorePassword=changeit",
		} {
			if !valuesContains(javaOpts, opt) {
				javaOpts = append(javaOpts, opt)
			}
		}
	}

	existingEnv, _ := vals["environment"].(map[string]interface{})
	if existingEnv == nil {
		existingEnv = map[string]interface{}{}
	}
	valuesMergeOverwrite(existingEnv, environment)
	listAt := func(key string) []interface{} {
		l, _ := vals[key].([]interface{})
		return append([]interface{}{}, l...)
	}
	valuesMergeOverwrite(vals, map[string]interface{}{
		"extraEnvVars":      append(listAt("extraEnvVars"), env...),
		"environment":       existingEnv,
		"extraVolumes":      append(listAt("extraVolumes"), volume()),
		"extraVolumeMounts": append(listAt("extraVolumeMounts"), mounts()...),
	})

	// A chart off the extraVolumes convention names its own keys, written
	// besides the conventional ones, never instead: a value it does not
	// read is inert.
	if vm := r.vm.Volumes; vm != nil {
		type write struct {
			path string
			add  []interface{}
		}
		var writes []write
		if vm.VolumesPath != "" {
			writes = append(writes, write{vm.VolumesPath, []interface{}{volume()}})
		}
		if vm.VolumeMountsPath != "" {
			writes = append(writes, write{vm.VolumeMountsPath, mounts()})
		}
		for _, w := range writes {
			parts := strings.Split(w.path, ".")
			existing, _ := valuesAt(vals, parts).([]interface{})
			built := append(append([]interface{}{}, existing...), w.add...)
			valuesMergeOverwrite(vals, valuesNest(parts, built))
		}
	}
	if needsJavaTrust {
		valuesMergeOverwrite(vals, map[string]interface{}{"javaOpts": javaOpts})
	}
}

// valuesRaw decodes a profile's free-form values; nil when there are none.
func valuesRaw(raw *runtime.RawExtension) (map[string]interface{}, error) {
	if raw == nil || len(raw.Raw) == 0 {
		return nil, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw.Raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// valuesAt reads what is at a dotted path of the values, nil when the path
// does not lead anywhere.
func valuesAt(vals map[string]interface{}, parts []string) interface{} {
	var node interface{} = vals
	for _, k := range parts {
		m, ok := node.(map[string]interface{})
		if !ok {
			return nil
		}
		if node, ok = m[k]; !ok {
			return nil
		}
	}
	return node
}

// valuesNest folds a dotted path into nested maps around a value: a flat
// "a.b.c" key would be merged as a literal key rather than a path.
func valuesNest(parts []string, value interface{}) map[string]interface{} {
	node := value
	for i := len(parts) - 1; i >= 0; i-- {
		node = map[string]interface{}{parts[i]: node}
	}
	return node.(map[string]interface{})
}

func valuesContains(list []interface{}, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// valuesMergeOverwrite merges src into dst the way sprig's mergeOverwrite
// (mergo with override) does: a map into a non-empty map key by key, and
// anything else -- a scalar, a list, a map over a scalar or an empty map --
// replacing what dst has.
func valuesMergeOverwrite(dst, src map[string]interface{}) {
	for k, sv := range src {
		if sm, ok := sv.(map[string]interface{}); ok {
			if dm, ok := dst[k].(map[string]interface{}); ok && len(dm) > 0 {
				valuesMergeOverwrite(dm, sm)
				continue
			}
		}
		dst[k] = sv
	}
}

// valuesCopy copies values read from the input, so that merging into them
// never changes the input.
func valuesCopy(m map[string]interface{}) map[string]interface{} {
	raw, _ := json.Marshal(m)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return out
}
