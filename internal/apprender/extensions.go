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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// renderExtensions renders each extension's release beside the app's: its
// own Helm release in the tenant's namespace, the Secret its values read and
// the stable Service it is routed to by.
//
// An extension is keyed {app}-{name} (api/v1alpha1.SidecarAppName) in
// OpenBao, the key its client and the purge on uninstall use as well. Only
// its generated secrets and its OIDC client reach its values: an extension
// that needs a database, a cache or object storage of its own is not
// supported. Its generated secrets are seeded by the tenant reconciler,
// as the app's are.
func renderExtensions(in *Input, out *Rendered) error {
	for i := range in.Profile.Extensions {
		ext := &in.Profile.Extensions[i]
		if ext.Name == "" || ext.Chart.Name == "" {
			continue
		}
		objs, err := extensionObjects(in, ext)
		if err != nil {
			return fmt.Errorf("extension %s: %w", ext.Name, err)
		}
		out.add(objs...)
	}
	return nil
}

func extensionObjects(in *Input, ext *gentianov1alpha1.AppSidecarSpec) ([]*unstructured.Unstructured, error) {
	ns, app, tenant := in.Namespace, in.App, in.Tenant()
	key := gentianov1alpha1.SidecarAppName(app, ext.Name)
	secretName := key + "-sensitive-values"

	esData := []interface{}{}
	set := []interface{}{}
	add := func(secretKey, remoteKey, property, valuePath string) {
		esData = append(esData, map[string]interface{}{
			"secretKey": secretKey,
			"remoteRef": map[string]interface{}{"key": remoteKey, "property": property},
		})
		set = append(set, map[string]interface{}{
			"name": valuePath,
			"valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{
				"name": secretName, "namespace": ns, "key": secretKey,
			}},
		})
	}
	for _, s := range ext.AppSecrets {
		if s.Name != "" && s.ValuePath != "" {
			add("internal-"+s.Name,
				fmt.Sprintf("gentian-os/tenants/%s/apps/%s/internal/%s", tenant, key, s.Name),
				"value", s.ValuePath)
		}
	}
	// The extension's OIDC client, mapped as the app's own: issuer and client
	// id at the extension's key, the secret at an explicit path where one is
	// named.
	if ext.ValueMapping != nil && ext.ValueMapping.OIDC != nil {
		vm := ext.ValueMapping.OIDC
		oidcPath := fmt.Sprintf("gentian-os/tenants/%s/apps/%s/oidc", tenant, key)
		if vm.IssuerKey != "" {
			add("oidc-issuer", oidcPath, "issuer", vm.IssuerKey)
		}
		if vm.ClientIDKey != "" {
			add("oidc-client-id", oidcPath, "client-id", vm.ClientIDKey)
		}
		if vm.ClientSecretKey != "" {
			add("oidc-client-secret",
				extensionOr(vm.ClientSecretOpenbaoPath, oidcPath),
				extensionOr(vm.ClientSecretOpenbaoProperty, "client-secret"),
				vm.ClientSecretKey)
		}
	}

	var objs []*unstructured.Unstructured
	if len(esData) > 0 {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata":   map[string]interface{}{"name": secretName, "namespace": ns},
			"spec": map[string]interface{}{
				"refreshInterval": "1h",
				"secretStoreRef": map[string]interface{}{
					"kind": "ClusterSecretStore",
					"name": "openbao-tenant-" + tenant,
				},
				"target": map[string]interface{}{
					"name":           secretName,
					"creationPolicy": "Owner",
					"deletionPolicy": "Retain",
				},
				"data": esData,
			},
		}})
	}

	values, err := extensionValues(in, ext)
	if err != nil {
		return nil, err
	}
	chart := map[string]interface{}{
		"repository": ext.Chart.Repository,
		"name":       ext.Chart.Name,
		"version":    ext.Chart.Version,
	}
	for _, c := range in.PullSecrets.Charts {
		if c.Repository == ext.Chart.Repository {
			chart["pullSecretRef"] = map[string]interface{}{"name": c.SecretName, "namespace": ns}
		}
	}
	forProvider := map[string]interface{}{
		"chart":     chart,
		"namespace": ns,
		// Ready only once the workloads are: a release that is Ready as soon
		// as Helm applied it reports a green app whose pods never schedule.
		// 10m because a first boot may migrate a schema.
		"wait":        true,
		"waitTimeout": "10m",
		"skipCRDs":    true,
		// The tenant's namespace exists, and the release's account may not
		// create one.
		"skipCreateNamespace": true,
		// The app's keep rule: the extension's files are the app's and are
		// kept on uninstall like the rest.
		"patchesFrom": []interface{}{
			map[string]interface{}{"configMapKeyRef": map[string]interface{}{
				"name":      in.KeepVolumes,
				"namespace": ns,
				"key":       "patch.yaml",
				"optional":  false,
			}},
		},
		"values": values,
	}
	if len(set) > 0 {
		forProvider["set"] = set
	}
	release := key + "-release"
	objs = append(objs, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "helm.crossplane.io/v1beta1",
		"kind":       "Release",
		"metadata": map[string]interface{}{
			"name": in.extensionReleaseObject(ext.Name),
			// One Helm release name for as long as the app is the tenant's,
			// as the app's own.
			"annotations": map[string]interface{}{"crossplane.io/external-name": release},
		},
		"spec": map[string]interface{}{
			"rollbackLimit": int64(3),
			"forProvider":   forProvider,
			// Installed as the tenant's narrow account, not the kernel's.
			"providerConfigRef": map[string]interface{}{"name": "tenant-apps"},
		},
	}})

	if ext.StableServiceName != "" {
		port := int64(ext.StableServicePort)
		if port == 0 {
			port = 80
		}
		objs = append(objs, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   map[string]interface{}{"name": ext.StableServiceName, "namespace": ns},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"app.kubernetes.io/instance": release},
				"ports": []interface{}{map[string]interface{}{
					"name":       "http",
					"port":       port,
					"targetPort": port,
					"protocol":   "TCP",
				}},
			},
		}})
	}
	return objs, nil
}

// extensionValues are the extension's extraValues with the tenant-identity
// tokens expanded (not the kernel endpoints: an extension reaches its
// dependencies as its app does), and the app's two pod labels: the tenant's
// kernel-access NetworkPolicy and resource attribution both select on them,
// and an extension is part of its app.
func extensionValues(in *Input, ext *gentianov1alpha1.AppSidecarSpec) (map[string]interface{}, error) {
	values := map[string]interface{}{}
	if ext.ExtraValues != nil && len(ext.ExtraValues.Raw) > 0 {
		// Expanded in the JSON text, as the Composition did, so a token is
		// replaced wherever it stands, keys included.
		var parsed interface{}
		if err := json.Unmarshal(ext.ExtraValues.Raw, &parsed); err != nil {
			return nil, fmt.Errorf("extraValues: %w", err)
		}
		if m, ok := parsed.(map[string]interface{}); ok && len(m) > 0 {
			raw, err := json.Marshal(m)
			if err != nil {
				return nil, err
			}
			pairs := []string{
				"${TENANT_DOMAIN}", in.TenantDomain(),
				"${TENANT_NAMESPACE}", in.Namespace,
				"${TENANT_ID}", in.Tenant(),
				"${APP_ID}", in.App,
				"${SIDECAR_ID}", ext.Name,
			}
			if d := in.KernelServices["KERNEL_DOMAIN"]; d != "" {
				pairs = append(pairs, "${KERNEL_DOMAIN}", d)
			}
			if ip := in.ClusterConfig["node.ip"]; ip != "" {
				pairs = append(pairs, "${NODE_IP}", ip)
			}
			text := string(raw)
			for i := 0; i < len(pairs); i += 2 {
				text = strings.ReplaceAll(text, pairs[i], pairs[i+1])
			}
			if err := json.Unmarshal([]byte(text), &values); err != nil {
				return nil, fmt.Errorf("extraValues after substitution: %w", err)
			}
		}
	}
	labels, _ := values["podLabels"].(map[string]interface{})
	if labels == nil {
		labels = map[string]interface{}{}
	}
	labels["gentianos.io/app"] = in.App
	labels["gentianos.io/tenant"] = in.Tenant()
	values["podLabels"] = labels
	return values, nil
}

// extensionOr is s, or def when s is empty.
func extensionOr(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
