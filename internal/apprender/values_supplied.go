/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// suppliedCredentials asks for each credential only a person can provide
// (spec.secrets.supplied): a CredentialRequirement of the tenant, which its
// Admin Console lists and the custodian writes to, and a probe beside it,
// an ExternalSecret that makes no Secret and exists for ESO's Ready
// condition, which the custodian reads. Rendered here so that neither can
// drift from the path the app's ExternalSecret reads.
func (r *valuesRender) suppliedCredentials() ([]*unstructured.Unstructured, error) {
	supplied := r.supplied()
	if len(supplied) == 0 {
		return nil, nil
	}
	// The probe goes where the custodian looks for every probe; one placed
	// anywhere else is a requirement that reads unsatisfied for ever.
	kernelNs := r.in.ClusterConfig["kernel.controlNamespace"]
	if kernelNs == "" {
		return nil, errors.New("cluster-config carries no kernel.controlNamespace, which the credential probes of a supplied secret are placed in")
	}
	app := r.in.App
	var objs []*unstructured.Unstructured
	for _, sc := range supplied {
		if sc.Name == "" || len(sc.Fields) == 0 {
			continue
		}
		reqName := fmt.Sprintf("app-%s-%s-%s", r.tenant, app, sc.Name)
		path := r.appPath("supplied/" + sc.Name)
		display := sc.DisplayName
		if display == "" {
			display = sc.Name
		}
		var fields, data []interface{}
		for _, f := range sc.Fields {
			if f.Key == "" || f.ValuePath == "" {
				continue
			}
			format := f.Format
			if format == "" {
				format = "string"
			}
			field := map[string]interface{}{"key": f.Key, "format": format, "secret": !f.Public}
			if f.Example != "" {
				field["example"] = f.Example
			}
			fields = append(fields, field)
			data = append(data, map[string]interface{}{
				"secretKey": f.Key,
				"remoteRef": map[string]interface{}{"key": path, "property": f.Key},
			})
		}

		req := valuesObject("gentianos.io/v1alpha1", "CredentialRequirement", reqName, "")
		// Tenant-scoped, and grouped under the app in the console by the
		// provider label.
		req.SetLabels(map[string]string{
			"gentianos.io/credential-phase":    "runtime",
			"gentianos.io/credential-scope":    "tenant",
			"gentianos.io/credential-provider": app,
			"gentianos.io/tenant":              r.tenant,
		})
		spec := map[string]interface{}{
			"displayName": display,
			"description": strings.TrimSpace(fmt.Sprintf("%s The app %s waits for it until it is set here.", strings.TrimSpace(sc.Description), app)),
			"phase":       "runtime",
			"scope":       "tenant",
			"tenant":      r.tenant,
			"vaultPath":   path,
			"validate":    map[string]interface{}{"type": "noop"},
			"consumedBy": []interface{}{map[string]interface{}{
				"kind": r.in.Consumer.Kind,
				"name": r.in.Consumer.Name,
			}},
		}
		// A credential with no mapped field has a null field list, as the
		// Composition wrote it.
		spec["fields"] = nil
		if fields != nil {
			spec["fields"] = fields
		}
		req.Object["spec"] = spec

		probe := valuesObject("external-secrets.io/v1", "ExternalSecret", "credreq-"+reqName, kernelNs)
		probe.SetLabels(map[string]string{
			"gentianos.io/credential-requirement": reqName,
			"gentianos.io/credential-phase":       "runtime",
			"gentianos.io/credential-scope":       "tenant",
		})
		probeSpec := map[string]interface{}{
			"refreshInterval": "1h",
			"secretStoreRef":  map[string]interface{}{"name": "openbao", "kind": "ClusterSecretStore"},
			"target":          map[string]interface{}{"creationPolicy": "None"},
			"data":            nil,
		}
		if data != nil {
			probeSpec["data"] = data
		}
		probe.Object["spec"] = probeSpec
		objs = append(objs, req, probe)
	}
	return objs, nil
}
