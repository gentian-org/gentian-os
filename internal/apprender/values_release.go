/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// valuesKeepPatch is the keep rule: a Kustomize patch, run as Helm's
// post-renderer, that marks every claim the chart renders for helm
// uninstall to leave in place. Uninstalling an app takes its workloads
// away and keeps its files; destroying them is a purge. The target names no
// object, so it matches every claim; the name inside the patch is required
// by the format and is not applied.
const valuesKeepPatch = `patches:
  - target:
      version: v1
      kind: PersistentVolumeClaim
    patch: |-
      apiVersion: v1
      kind: PersistentVolumeClaim
      metadata:
        name: every-claim-of-the-release
        annotations:
          helm.sh/resource-policy: keep
`

// valuesPodLabelKinds are the kinds with a pod template that the account a
// tenant's chart is installed as may write (gentian-tenant-apps-helm).
var valuesPodLabelKinds = []struct{ group, kind string }{
	{"apps", "Deployment"},
	{"apps", "StatefulSet"},
	{"batch", "Job"},
}

// valuesPodLabelsPatch is the label rule: one patch per kind with a pod
// template, run after the chart is rendered, that sets the app's and the
// tenant's label on every pod template of the release. The tenant's network
// policies select an app's pods by these labels, so which app a pod belongs
// to is said here, by the operator, and not by the chart: whatever a chart
// writes under either key is replaced. As in the keep rule, a target names
// no object and so matches every one of its kind.
func valuesPodLabelsPatch(app, tenant string) string {
	var b strings.Builder
	for _, k := range valuesPodLabelKinds {
		fmt.Fprintf(&b, `  - target:
      group: %[1]s
      version: v1
      kind: %[2]s
    patch: |-
      apiVersion: %[1]s/v1
      kind: %[2]s
      metadata:
        name: every-%[3]s-of-the-release
      spec:
        template:
          metadata:
            labels:
              gentianos.io/app: %[4]q
              gentianos.io/tenant: %[5]q
`, k.group, k.kind, strings.ToLower(k.kind), app, tenant)
	}
	return b.String()
}

// releasePatch is what the release's post-renderer applies: the keep rule
// and the label rule, for every chart. A chart the platform places is told
// nothing of the labels in its values; its pods carry them all the same.
func (r *valuesRender) releasePatch() string {
	return valuesKeepPatch + valuesPodLabelsPatch(r.in.App, r.tenant)
}

// keepVolumes is the ConfigMap the release reads its patch from.
func (r *valuesRender) keepVolumes() *unstructured.Unstructured {
	cm := valuesObject("v1", "ConfigMap", r.in.KeepVolumes, r.ns)
	cm.Object["data"] = map[string]interface{}{"patch.yaml": r.releasePatch()}
	return cm
}

// release is the provider-helm Release that installs the app's chart.
func (r *valuesRender) release(v *valuesSensitive, values map[string]interface{}) *unstructured.Unstructured {
	chart := r.spec.Package.Chart
	rel := valuesObject("helm.crossplane.io/v1beta1", "Release", r.in.ReleaseObject, "")
	// The Helm release name; without it the provider uses the object's.
	rel.SetAnnotations(map[string]string{"crossplane.io/external-name": r.in.HelmRelease})

	chartSpec := map[string]interface{}{
		"repository": chart.Repository,
		"name":       chart.Name,
		"version":    chart.Version,
	}
	// A chart inside a repository the tenant declared is pulled with that
	// repository's credential, from the app's own namespace.
	for _, c := range r.in.PullSecrets.Charts {
		if c.Repository == chart.Repository {
			chartSpec["pullSecretRef"] = map[string]interface{}{"name": c.SecretName, "namespace": r.ns}
		}
	}
	forProvider := map[string]interface{}{
		"chart":     chartSpec,
		"namespace": r.ns,
		// Ready once the workloads are, not once Helm applied the
		// manifests: an app whose pods never schedule must not read Ready.
		// 10m for a first boot's migrations.
		"wait":        true,
		"waitTimeout": releaseWaitTimeout,
		"skipCRDs":    true,
		// The tenant's namespace exists, and the release's account may not
		// create one.
		"skipCreateNamespace": true,
		// Not optional, which is what a reference that does not say is: a
		// release installed without the rule would delete the app's files
		// when uninstalled. See releaseWaitTimeout for why it does not say.
		"patchesFrom": []interface{}{map[string]interface{}{
			"configMapKeyRef": map[string]interface{}{
				"name":      r.in.KeepVolumes,
				"namespace": r.ns,
				"key":       "patch.yaml",
			},
		}},
	}
	if len(v.valuesFrom) > 0 {
		forProvider["valuesFrom"] = v.valuesFrom
	}
	if len(values) > 0 {
		forProvider["values"] = values
	}
	if len(v.set) > 0 {
		forProvider["set"] = v.set
	}
	rel.Object["spec"] = map[string]interface{}{
		"rollbackLimit": int64(3),
		"forProvider":   forProvider,
		// A tenant's chart is installed as the narrow account, not as the
		// kernel's cluster-admin one.
		"providerConfigRef": map[string]interface{}{"name": "tenant-apps"},
	}
	return rel
}

// serviceAliases are stable Services under the names the profile routes
// to, over the chart's own: the first gateway exposure's backend and the
// companion <app>-api. None when the chart names its Service so itself
// (fullnameOverride). One the chart makes under fullnameOverride-<component>
// is only observed: made while the release still waits it would be a
// Service Helm refuses to install over, and dropping it instead would
// delete a Service an earlier alias already took over.
func (r *valuesRender) serviceAliases(values map[string]interface{}) (objs, observe []*unstructured.Unstructured) {
	var service string
	var port int32
	for _, e := range r.spec.Expose {
		if e.Surface == "gateway" && service == "" {
			service, port = e.Backend.Service, e.Backend.Port
		}
	}
	fullnameOverride, _ := values["fullnameOverride"].(string)
	if service == "" || service == fullnameOverride {
		return nil, nil
	}
	if port == 0 {
		port = 80
	}
	chartsOwn := fullnameOverride + "-"
	alias := func(name, component string, port int64, chartMakesIt bool) {
		svc := valuesObject("v1", "Service", name, r.ns)
		svc.Object["spec"] = map[string]interface{}{
			"selector": map[string]interface{}{
				"app.kubernetes.io/instance":  r.in.HelmRelease,
				"app.kubernetes.io/component": component,
			},
			"ports": []interface{}{map[string]interface{}{
				"name":       "http",
				"port":       port,
				"targetPort": port,
				"protocol":   "TCP",
			}},
		}
		if chartMakesIt {
			observe = append(observe, svc)
		} else {
			objs = append(objs, svc)
		}
	}
	alias(service, strings.TrimPrefix(service, r.in.App+"-"), int64(port),
		fullnameOverride != "" && strings.HasPrefix(service, chartsOwn))
	// The API alias is the chart's own only when it is exactly
	// <fullnameOverride>-api: an app named past its override (fellwort-ee,
	// override fellwort) starts with it too, but its chart makes
	// fellwort-api, and an observed alias nobody creates is never there.
	api := r.in.App + "-api"
	alias(api, "api", 8000, fullnameOverride != "" && api == fullnameOverride+"-api")
	return objs, observe
}

// releaseWaitTimeout is how long a release waits for its workloads, written
// the way the Helm provider writes it back.
//
// The provider rewrites a Release whenever it records an attempt on it, and
// writes its own spelling of what it read: a duration as Go prints it, and
// no "optional: false", which it leaves out as the default. A Release
// rendered in any other spelling differs from the live one after every
// failed attempt, is put back, and the write starts the next attempt at
// once: a release that cannot install was retried several times a second,
// each time signing in to its registry. So the render says both the
// provider's way.
const releaseWaitTimeout = "10m0s"
