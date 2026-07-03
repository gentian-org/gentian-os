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

package applifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

var argoApplicationGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Application",
}

func (s *Service) reconcileTenantFile(ctx context.Context, file string, waitArgoSync bool) error {
	instance, stage := instanceStageFromPath(file)
	if instance != "" && stage != "" {
		appNS, appName, found, err := s.findArgoTenantApp(ctx, instance, stage)
		if err != nil {
			return err
		}
		if found {
			return s.triggerArgoSyncPrune(ctx, appNS, appName, waitArgoSync)
		}
	}
	return s.applyTenantFile(ctx, file)
}

func instanceStageFromPath(file string) (instance, stage string) {
	re := regexp.MustCompile(`/tenants/([^/]+)/([^/]+)/`)
	m := re.FindStringSubmatch(filepath.ToSlash(file))
	if len(m) == 3 {
		return m[1], m[2]
	}
	return "", ""
}

func (s *Service) findArgoTenantApp(ctx context.Context, instance, stage string) (namespace, name string, found bool, err error) {
	cluster := s.opts.DeploymentsCluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	wantPath := fmt.Sprintf("clusters/%s/tenants/%s/%s", cluster, instance, stage)

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(argoApplicationGVK)
	if err := s.client.List(ctx, list); err != nil {
		return "", "", false, err
	}
	for _, item := range list.Items {
		path, _, _ := unstructured.NestedString(item.Object, "spec", "source", "path")
		if path == wantPath {
			return item.GetNamespace(), item.GetName(), true, nil
		}
	}
	return "", "", false, nil
}

func (s *Service) triggerArgoSyncPrune(ctx context.Context, ns, name string, waitSync bool) error {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argoApplicationGVK)
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := s.client.Get(ctx, key, app); err != nil {
		return err
	}

	ann := app.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann["argocd.argoproj.io/refresh"] = "hard"
	app.SetAnnotations(ann)
	if err := s.client.Update(ctx, app); err != nil {
		return fmt.Errorf("argocd refresh annotation: %w", err)
	}

	patch := []byte(`{"operation":{"initiatedBy":{"username":"applifecycle"},"sync":{"prune":true}}}`)
	if err := s.client.Patch(ctx, app, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("argocd sync patch: %w", err)
	}
	if !waitSync {
		return nil
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.client.Get(ctx, key, app); err != nil {
			return err
		}
		status, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
		if status == "Synced" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

func (s *Service) applyTenantFile(ctx context.Context, file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(b, &obj.Object); err != nil {
		return fmt.Errorf("decode tenant manifest: %w", err)
	}
	return s.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner("gentian-applifecycle"), client.ForceOwnership)
}
