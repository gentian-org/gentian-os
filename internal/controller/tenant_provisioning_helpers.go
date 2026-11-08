/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const tenantProvisioningObjectsDataKey = "objects.json"

func serializeProvisioningObjects(objects []client.Object) (string, error) {
	rawObjects := make([]json.RawMessage, 0, len(objects))
	for _, obj := range objects {
		uMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return "", err
		}
		if _, ok := uMap["apiVersion"]; !ok {
			gvk := obj.GetObjectKind().GroupVersionKind()
			if gvk.Empty() {
				return "", fmt.Errorf("object %T has no GroupVersionKind", obj)
			}
			uMap["apiVersion"] = gvk.GroupVersion().String()
			uMap["kind"] = gvk.Kind
		}
		raw, err := json.Marshal(uMap)
		if err != nil {
			return "", err
		}
		rawObjects = append(rawObjects, raw)
	}
	payload, err := json.Marshal(rawObjects)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}
