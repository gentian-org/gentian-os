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

package authz

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Lookup finds the store and its current model without writing either: for
// a reader such as the ext-auth shim, which must decide against what the
// director established and never establish anything of its own.
func Lookup(ctx context.Context, o Options) (storeID, modelID string, err error) {
	c, err := newClient(o)
	if err != nil {
		return "", "", err
	}
	found, err := c.storesNamed(ctx, StoreName)
	if err != nil {
		return "", "", err
	}
	if len(found) == 0 {
		return "", "", errors.New("authz: no authorization store yet; the director creates it")
	}
	sort.Slice(found, func(i, j int) bool { return found[i].CreatedAt.Before(found[j].CreatedAt) })
	storeID = found[0].ID
	var latest struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"authorization_models"`
	}
	if err := c.get(ctx, "/stores/"+storeID+"/authorization-models?page_size=1", &latest); err != nil {
		return "", "", err
	}
	if len(latest.Models) == 0 {
		return "", "", fmt.Errorf("authz: store %s has no model yet; the director writes it", storeID)
	}
	return storeID, latest.Models[0].ID, nil
}
