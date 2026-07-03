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
	"encoding/json"
	"fmt"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
)

const (
	syncedTupleKeysAnnotation = "gentianos.io/synced-tuple-keys"
	appGrantFinalizer         = "gentianos.io/app-grant-cleanup"
)

func syncedTupleKeysFromAnnotation(raw string) ([]authz.TupleKey, error) {
	if raw == "" {
		return nil, nil
	}
	var keys []authz.TupleKey
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil, fmt.Errorf("parse synced tuple keys: %w", err)
	}
	return keys, nil
}

func encodeSyncedTupleKeys(keys []authz.TupleKey) (string, error) {
	b, err := json.Marshal(keys)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func grantTupleSyncPlan(
	tenantName string,
	grant *gentianov1alpha1.AppGrant,
	prevKeys []authz.TupleKey,
) (writes []authz.Tuple, deletes []authz.TupleKey, nextKeys []authz.TupleKey) {
	desiredTuples := authz.GrantTuples(tenantName, grant)
	desiredKeys := authz.TupleKeysFromTuples(desiredTuples)
	deletes = authz.TupleKeysNotIn(prevKeys, desiredKeys)
	writeKeys := authz.TupleKeysNotIn(desiredKeys, prevKeys)
	writes = authz.TuplesMatchingKeys(desiredTuples, writeKeys)
	return writes, deletes, desiredKeys
}
