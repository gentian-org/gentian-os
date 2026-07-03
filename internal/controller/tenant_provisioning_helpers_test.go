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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestAppUsesCrossplaneS3Init(t *testing.T) {
	t.Parallel()

	profile := &gentianov1alpha1.AppProfile{
		Spec: gentianov1alpha1.AppProfileSpec{
			DeploymentMethod: gentianov1alpha1.DeploymentMethodCrossplane,
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Storage: &gentianov1alpha1.StorageRequirement{
					S3: &gentianov1alpha1.S3Requirement{BucketPerTenant: true},
				},
			},
		},
	}
	if !appUsesCrossplaneS3Init(profile) {
		t.Fatal("expected crossplane app with bucketPerTenant to use composition s3-init")
	}

	withCompositionRef := *profile
	withCompositionRef.Spec.CompositionRef = "app-custom"
	if !appUsesCrossplaneS3Init(&withCompositionRef) {
		t.Fatal("expected explicit compositionRef to remain supported")
	}

	nonCrossplane := *profile
	nonCrossplane.Spec.DeploymentMethod = gentianov1alpha1.DeploymentMethodArgoCD
	if appUsesCrossplaneS3Init(&nonCrossplane) {
		t.Fatal("expected non-crossplane app to skip composition s3-init")
	}
}
