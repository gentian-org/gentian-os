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
