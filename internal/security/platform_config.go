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


package security

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const operatorNamespace = "gentian-system"

// LoadAllowedMacWaivers reads the cluster PlatformSecurityPolicy singleton.
func LoadAllowedMacWaivers(ctx context.Context, c client.Client) ([]gentianov1alpha1.AllowedMacWaiver, error) {
	psp := &gentianov1alpha1.PlatformSecurityPolicy{}
	err := c.Get(ctx, types.NamespacedName{Name: gentianov1alpha1.PlatformSecurityPolicyName}, psp)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get PlatformSecurityPolicy: %w", err)
	}
	return psp.Spec.AllowedMacWaivers, nil
}

// SyncPlatformSecurityConfigMap writes the cluster allowlist for compositions.
func SyncPlatformSecurityConfigMap(
	ctx context.Context,
	c client.Client,
	allowed []gentianov1alpha1.AllowedMacWaiver,
) error {
	payload, err := json.Marshal(allowed)
	if err != nil {
		return fmt.Errorf("marshal allowed mac waivers: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gentianov1alpha1.PlatformSecurityConfigMapName,
			Namespace: operatorNamespace,
			Labels: map[string]string{
				meta.ManagedByLabel:              meta.ManagedByValue,
				"gentianos.io/config-type":       gentianov1alpha1.PlatformSecurityConfigTypeLabel,
			},
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[gentianov1alpha1.PlatformSecurityConfigMapKey] = string(payload)
		return nil
	})
	if err != nil {
		return fmt.Errorf("sync platform security ConfigMap: %w", err)
	}
	return nil
}

// ParseAllowedMacWaiversFromConfigMap decodes the platform security ConfigMap payload.
func ParseAllowedMacWaiversFromConfigMap(data string) ([]gentianov1alpha1.AllowedMacWaiver, error) {
	if data == "" {
		return nil, nil
	}
	var allowed []gentianov1alpha1.AllowedMacWaiver
	if err := json.Unmarshal([]byte(data), &allowed); err != nil {
		return nil, fmt.Errorf("decode allowed mac waivers: %w", err)
	}
	return allowed, nil
}
