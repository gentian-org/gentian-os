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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestProvisioningJobObjectName(t *testing.T) {
	t.Parallel()
	got := provisioningJobObjectName("demo", "keycloak-gentian-groups-demo")
	if got != "demo-job-keycloak-gentian-groups-demo" {
		t.Fatalf("got %q", got)
	}
}

func TestCrossplaneObjectReady(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{}
	obj.SetUnstructuredContent(map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": string(metav1.ConditionTrue),
				},
			},
		},
	})
	if !crossplaneObjectReady(obj) {
		t.Fatal("expected Object Ready=True")
	}
	obj.SetUnstructuredContent(map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": string(metav1.ConditionFalse),
				},
			},
		},
	})
	if crossplaneObjectReady(obj) {
		t.Fatal("expected Object Ready=False")
	}
}
