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
