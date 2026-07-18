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

func (s *Service) waitForAppUninstalled(ctx context.Context, tenant, profile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ns := tenantNamespace(tenant)
	for {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(appClaimGVK)
		err := s.client.Get(ctx, client.ObjectKey{Name: profile, Namespace: ns}, obj)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			msg, _, _ := unstructured.NestedString(obj.Object, "status", "conditions", "0", "message")
			return fmt.Errorf("timed out waiting for app %q to be removed: %s", profile, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
