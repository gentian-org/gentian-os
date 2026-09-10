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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *Service) waitForAppReady(ctx context.Context, tenant, profile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ready, msg, err := s.appReadyState(ctx, tenant, profile)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for app %q to become Ready: %s", profile, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForAppUninstalled blocks until the App claim and the composite behind it
// are both gone.
//
// The claim disappearing is not the end of the teardown. Crossplane deletes the
// XApp and everything it composed — the Helm Release, the Objects, and through
// them the workloads still holding PVCs — after the claim has already gone. A
// purge that starts at that moment races Crossplane for the same resources, and
// an install issued right after one returns lands on a namespace that is still
// emptying: the fresh claim binds while the previous composite is terminating,
// and the new release's volumes and secrets are removed by the tail of the old
// teardown. Waiting for the composite closes that window.
func (s *Service) waitForAppUninstalled(ctx context.Context, tenant, profile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ns := tenantNamespace(tenant)

	// Read the composite's name up front: once the claim is gone the reference
	// to it is gone too, and an orphaned composite has nothing pointing back.
	xrName := s.compositeNameFor(ctx, tenant, profile)

	for {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(appClaimGVK)
		err := s.client.Get(ctx, client.ObjectKey{Name: profile, Namespace: ns}, obj)
		switch {
		case apierrors.IsNotFound(err):
			if xrName == "" {
				return nil
			}
			gone, err := s.compositeGone(ctx, xrName)
			if err != nil {
				return err
			}
			if gone {
				return nil
			}
		case err != nil:
			return err
		}
		if time.Now().After(deadline) {
			msg, _, _ := unstructured.NestedString(obj.Object, "status", "conditions", "0", "message")
			if msg == "" && xrName != "" {
				msg = fmt.Sprintf("composite %s is still terminating", xrName)
			}
			return fmt.Errorf("timed out waiting for app %q to be removed: %s", profile, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// compositeNameFor returns the XApp backing this claim, or "" when the claim is
// already gone or has not been bound to one.
func (s *Service) compositeNameFor(ctx context.Context, tenant, profile string) string {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimGVK)
	if err := s.client.Get(ctx,
		client.ObjectKey{Name: profile, Namespace: tenantNamespace(tenant)}, obj); err != nil {
		return ""
	}
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "resourceRef", "name")
	return name
}

func (s *Service) compositeGone(ctx context.Context, name string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(xAppGVK)
	err := s.client.Get(ctx, client.ObjectKey{Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		// A composite whose CRD is absent cannot be waited on; treat that as gone
		// rather than blocking the uninstall on a type that does not exist.
		if meta.IsNoMatchError(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
