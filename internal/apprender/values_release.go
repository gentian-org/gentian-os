/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
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

// keepVolumes is the ConfigMap the release reads its keep rule from.
func (r *valuesRender) keepVolumes() *unstructured.Unstructured {
	cm := valuesObject("v1", "ConfigMap", r.in.KeepVolumes, r.ns)
	cm.Object["data"] = map[string]interface{}{"patch.yaml": valuesKeepPatch}
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
		"waitTimeout": "10m",
		"skipCRDs":    true,
		// The tenant's namespace exists, and the release's account may not
		// create one.
		"skipCreateNamespace": true,
		// Not optional: a release installed without the rule would delete
		// the app's files when uninstalled.
		"patchesFrom": []interface{}{map[string]interface{}{
			"configMapKeyRef": map[string]interface{}{
				"name":      r.in.KeepVolumes,
				"namespace": r.ns,
				"key":       "patch.yaml",
				"optional":  false,
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
