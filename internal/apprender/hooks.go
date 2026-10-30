/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// hooksEnvVar is one fixed environment variable of the post-install job,
// read from a key of the app's sensitive-values Secret.
type hooksEnvVar struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// renderHooks renders the app's post-install job (hooks.postInstall): the
// bootstrap that has to go through the app's own admin API rather than Helm
// values. A failing pod is retried, so the script is idempotent and exits
// non-zero while a dependency is not ready yet.
func renderHooks(in *Input, out *Rendered) error {
	if in.Profile.Hooks == nil || in.Profile.Hooks.PostInstall == nil || in.Profile.Hooks.PostInstall.Image == "" {
		return nil
	}
	pij := in.Profile.Hooks.PostInstall
	ns, app, tenant := in.Namespace, in.App, in.Tenant()

	// The same tokens extraValues are expanded with: a token left in the
	// script reaches the container as literal text, and `set -u` kills it.
	script := pij.Script
	for _, r := range [][2]string{
		{"${TENANT_DOMAIN}", in.TenantDomain()},
		{"${TENANT_NAMESPACE}", ns},
		{"${TENANT_ID}", tenant},
		{"${APP_ID}", app},
	} {
		script = strings.ReplaceAll(script, r[0], r[1])
	}
	env := hooksEnv(in.Profile)

	// The Job runs once, and again only when what it runs changes. Its pod
	// template is immutable and a finished Job is not deleted (a TTL made it
	// run again every time it was recreated), so the digest of everything
	// it runs is in its name: a change is a new Job, and the old one goes.
	digest, err := hooksDigest(pij, script, env)
	if err != nil {
		return err
	}

	secretName := app + "-sensitive-values"
	scriptsCM := app + "-post-install"
	labels := map[string]interface{}{
		"app.kubernetes.io/managed-by": "gentian-os",
		"gentianos.io/tenant":          tenant,
		"gentianos.io/app":             app,
	}
	podLabels := map[string]interface{}{"gentianos.io/component": "post-install"}
	for k, v := range labels {
		podLabels[k] = v
	}

	volumes := []interface{}{
		map[string]interface{}{"name": "scripts", "configMap": map[string]interface{}{"name": scriptsCM}},
	}
	mounts := []interface{}{
		map[string]interface{}{"name": "scripts", "mountPath": "/scripts"},
	}
	container := map[string]interface{}{
		"name":  "post-install",
		"image": pij.Image,
		"securityContext": map[string]interface{}{
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
			"seccompProfile":           map[string]interface{}{"type": "RuntimeDefault"},
		},
		"command": []interface{}{"/bin/sh", "/scripts/run.sh"},
	}
	podSpec := map[string]interface{}{
		"restartPolicy":   "OnFailure",
		"securityContext": map[string]interface{}{"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"}},
	}
	if pij.ServiceAccountName != "" {
		podSpec["serviceAccountName"] = pij.ServiceAccountName
	}
	if pvc := pij.ReadOnlyPVC; pvc != nil && pvc.ClaimName != "" {
		// Run where the volume already is. The app's claim is ReadWriteOnce
		// on every StorageClass the platform installs, so a pod on another
		// node sits in ContainerCreating for ever, which no backoffLimit
		// bounds. Required, not preferred: Pending is visible and retried
		// the moment the app pod runs. Matched on the platform's own labels,
		// excluding an earlier post-install pod.
		podSpec["affinity"] = map[string]interface{}{
			"podAffinity": map[string]interface{}{
				"requiredDuringSchedulingIgnoredDuringExecution": []interface{}{
					map[string]interface{}{
						"topologyKey": "kubernetes.io/hostname",
						"labelSelector": map[string]interface{}{
							"matchLabels": map[string]interface{}{
								"gentianos.io/tenant": tenant,
								"gentianos.io/app":    app,
							},
							"matchExpressions": []interface{}{
								map[string]interface{}{
									"key":      "gentianos.io/component",
									"operator": "NotIn",
									"values":   []interface{}{"post-install"},
								},
							},
						},
					},
				},
			},
		}
		volumes = append(volumes, map[string]interface{}{
			"name": "readonly-data",
			"persistentVolumeClaim": map[string]interface{}{
				"claimName": pvc.ClaimName,
				"readOnly":  true,
			},
		})
		mounts = append(mounts, map[string]interface{}{
			"name":      "readonly-data",
			"mountPath": pvc.MountPath,
			"readOnly":  true,
		})
	}
	container["volumeMounts"] = mounts
	if len(env) > 0 {
		vars := make([]interface{}, 0, len(env))
		for _, e := range env {
			vars = append(vars, map[string]interface{}{
				"name": e.Name,
				"valueFrom": map[string]interface{}{
					"secretKeyRef": map[string]interface{}{"name": secretName, "key": e.Key},
				},
			})
		}
		container["env"] = vars
	}
	if len(pij.EnvFrom) > 0 {
		refs := make([]interface{}, 0, len(pij.EnvFrom))
		for _, name := range pij.EnvFrom {
			refs = append(refs, map[string]interface{}{"secretRef": map[string]interface{}{"name": name}})
		}
		container["envFrom"] = refs
	}
	podSpec["volumes"] = volumes
	podSpec["containers"] = []interface{}{container}

	cm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": scriptsCM, "namespace": ns},
		"data":       map[string]interface{}{"run.sh": hooksLiteralBlock(script)},
	}}
	job := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]interface{}{
			"name":      fmt.Sprintf("%s-post-install-%s", app, digest),
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]interface{}{
			"backoffLimit": int64(10),
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": podLabels},
				"spec":     podSpec,
			},
		},
	}}
	out.add(cm, job)
	return nil
}

// hooksEnv are the post-install job's fixed environment variables. envFrom
// cannot carry the sensitive-values keys: Kubernetes drops every key that is
// not a C identifier, which every hyphenated one is. These names are the
// documented contract for a script, whatever Helm keys the profile maps.
func hooksEnv(p *gentianov1alpha1.ComponentProfileSpec) []hooksEnvVar {
	env := []hooksEnvVar{}
	add := func(set bool, name, key string) {
		if set {
			env = append(env, hooksEnvVar{Name: name, Key: key})
		}
	}
	if vm := p.Package.ValueMapping; vm != nil {
		if m := vm.Database; m != nil {
			add(m.HostKey != "", "DB_HOST", "db-host")
			add(m.PortKey != "", "DB_PORT", "db-port")
			add(m.NameKey != "", "DB_NAME", "db-name")
			add(m.UserKey != "", "DB_USER", "db-user")
			add(m.PasswordKey != "", "DB_PASSWORD", "db-password")
		}
		if m := vm.OIDC; m != nil {
			add(m.IssuerKey != "", "OIDC_ISSUER", "oidc-issuer")
			add(m.ClientIDKey != "", "OIDC_CLIENT_ID", "oidc-client-id")
			add(m.ClientSecretKey != "", "OIDC_CLIENT_SECRET", "oidc-client-secret")
		}
		if m := vm.S3; m != nil {
			add(m.EndpointKey != "", "S3_ENDPOINT", "s3-endpoint")
			add(m.BucketKey != "", "S3_BUCKET", "s3-bucket")
			add(m.AccessKeyKey != "", "S3_ACCESS_KEY", "s3-access-key")
			add(m.SecretKeyKey != "", "S3_SECRET_KEY", "s3-secret-key")
			add(m.RegionKey != "", "S3_REGION", "s3-region")
		}
		if m := vm.Cache; m != nil {
			add(m.HostKey != "", "CACHE_HOST", "cache-host")
			add(m.PortKey != "", "CACHE_PORT", "cache-port")
			add(m.UserKey != "", "CACHE_USER", "cache-user")
			add(m.PasswordKey != "", "CACHE_PASSWORD", "cache-password")
		}
		// The model gateway's record is written only for a profile that
		// declares requires.services.llm, so only such a profile maps it.
		if m := vm.LLM; m != nil && hooksDeclaresLLM(p) {
			add(m.BaseURLKey != "", "LLM_BASE_URL", "llm-base-url")
			add(m.APIKeyKey != "", "LLM_API_KEY", "llm-api-key")
		}
		if m := vm.SMTP; m != nil {
			add(m.HostKey != "", "SMTP_HOST", "smtp-host")
			add(m.PortKey != "", "SMTP_PORT", "smtp-port")
			add(m.UserKey != "", "SMTP_USER", "smtp-user")
			add(m.PasswordKey != "", "SMTP_PASSWORD", "smtp-password")
		}
		if m := vm.IMAP; m != nil {
			add(m.HostKey != "", "IMAP_HOST", "imap-host")
			add(m.PortKey != "", "IMAP_PORT", "imap-port")
		}
	}
	// A generated secret by its declared name, upper-cased.
	if p.Secrets != nil {
		for _, s := range p.Secrets.Generated {
			add(s.Name != "", strings.ToUpper(s.Name), "internal-"+s.Name)
		}
	}
	return env
}

// hooksDeclaresLLM is whether the profile declares the model gateway: the
// requirement's presence is the declaration.
func hooksDeclaresLLM(p *gentianov1alpha1.ComponentProfileSpec) bool {
	return p.Requires != nil && p.Requires.Services != nil && p.Requires.Services.LLM != nil
}

// hooksDigest is the first eight hex digits of the SHA-256 of everything the
// job runs, as JSON with sorted keys: the digest the Composition named its
// Job by, so that a Job it made is not made again.
func hooksDigest(pij *gentianov1alpha1.AppPostInstallJob, script string, env []hooksEnvVar) (string, error) {
	envFrom := []string{}
	if pij.EnvFrom != nil {
		envFrom = pij.EnvFrom
	}
	var pvc interface{} = map[string]interface{}{}
	if pij.ReadOnlyPVC != nil {
		pvc = pij.ReadOnlyPVC
	}
	run := map[string]interface{}{
		"image":              pij.Image,
		"script":             script,
		"env":                env,
		"envFrom":            envFrom,
		"serviceAccountName": pij.ServiceAccountName,
		"readOnlyPVC":        pvc,
	}
	raw, err := json.Marshal(run)
	if err != nil {
		return "", fmt.Errorf("post-install job digest: %w", err)
	}
	// env entries are marshaled as sorted-key maps, as the template's dicts
	// were: {"key":...,"name":...}. hooksEnvVar's field order is name, key,
	// so it is re-encoded through a map.
	var canonical interface{}
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return "", err
	}
	if raw, err = json.Marshal(canonical); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:8], nil
}

// hooksLiteralBlock is the script as a YAML literal block scalar (`|`) reads
// it back: trailing newlines clipped to one.
func hooksLiteralBlock(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}
