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

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// loadAppProfileIndex lists cluster AppProfiles once per tenant reconcile.
func loadAppProfileIndex(ctx context.Context, c client.Client) (map[string]*gentianov1alpha1.AppProfile, error) {
	list := &gentianov1alpha1.AppProfileList{}
	if err := c.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list AppProfiles: %w", err)
	}
	index := make(map[string]*gentianov1alpha1.AppProfile, len(list.Items))
	for i := range list.Items {
		index[list.Items[i].Name] = &list.Items[i]
	}
	return index, nil
}

func appProfileFromIndex(index map[string]*gentianov1alpha1.AppProfile, name string) (*gentianov1alpha1.AppProfile, bool) {
	if index == nil || name == "" {
		return nil, false
	}
	profile, ok := index[name]
	return profile, ok
}
