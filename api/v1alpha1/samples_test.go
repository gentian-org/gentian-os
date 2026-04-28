package v1alpha1_test

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/api/v1alpha1"
)

const samplesDir = "../../config/samples"

func sampleScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic("AddToScheme: " + err.Error())
	}
	return s
}

func TestSampleFiles_ParseAndValidate(t *testing.T) {
	scheme := sampleScheme()
	codecFactory := serializer.NewCodecFactory(scheme)
	decoder := codecFactory.UniversalDeserializer()

	entries, err := os.ReadDir(samplesDir)
	if err != nil {
		t.Fatalf("cannot read samples dir %q: %v", samplesDir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".yaml" {
			continue
		}

		t.Run(name, func(t *testing.T) {
			path := filepath.Join(samplesDir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %q: %v", path, err)
			}

			jsonBytes, err := yaml.YAMLToJSON(raw)
			if err != nil {
				t.Fatalf("yaml to json %q: %v", name, err)
			}

			obj, gvk, err := decoder.Decode(jsonBytes, nil, nil)
			if err != nil {
				t.Fatalf("decode %q: %v", name, err)
			}

			if gvk.Group != "gentianos.io" {
				t.Errorf("expected group gentianos.io, got %q", gvk.Group)
			}
			if gvk.Version != "v1alpha1" {
				t.Errorf("expected version v1alpha1, got %q", gvk.Version)
			}

			switch o := obj.(type) {
			case *v1alpha1.AppProfile:
				validateAppProfile(t, o)
			case *v1alpha1.Tenant:
				validateTenant(t, o)
			case *v1alpha1.IntegrationBinding:
				validateIntegrationBinding(t, o)
			default:
				t.Errorf("unexpected type %T", obj)
			}
		})
	}
}

func validateAppProfile(t *testing.T, ap *v1alpha1.AppProfile) {
	t.Helper()
	if ap.Name == "" {
		t.Error("AppProfile: metadata.name is empty")
	}
	if ap.Spec.DisplayName == "" {
		t.Error("AppProfile: spec.displayName is empty")
	}
	if ap.Spec.Chart.Repository == "" {
		t.Error("AppProfile: spec.chart.repository is empty")
	}
	if ap.Spec.Chart.Name == "" {
		t.Error("AppProfile: spec.chart.name is empty")
	}
	if ap.Spec.Chart.Version == "" {
		t.Error("AppProfile: spec.chart.version is empty")
	}
	for i, s := range ap.Spec.AppSecrets {
		if s.Name == "" {
			t.Errorf("AppProfile %q appSecrets[%d]: name is empty", ap.Name, i)
		}
		if s.ValuePath == "" {
			t.Errorf("AppProfile %q appSecrets[%d]: valuePath is empty", ap.Name, i)
		}
	}
	switch ap.Spec.DeploymentMethod {
	case v1alpha1.DeploymentMethodArgoCD, v1alpha1.DeploymentMethodTofuController, "":
	default:
		t.Errorf("AppProfile %q: unknown deploymentMethod %q", ap.Name, ap.Spec.DeploymentMethod)
	}
}

func validateTenant(t *testing.T, tenant *v1alpha1.Tenant) {
	t.Helper()
	if tenant.Name == "" {
		t.Error("Tenant: metadata.name is empty")
	}
	if tenant.Spec.DisplayName == "" {
		t.Error("Tenant: spec.displayName is empty")
	}
	// spec.domain is optional (vanity domain). When unset, the operator
	// falls back to <tenant>.<KERNEL_DOMAIN>. See tenant_types.go.
	if tenant.Spec.AdminEmail == "" {
		t.Error("Tenant: spec.adminEmail is empty")
	}
	for i, app := range tenant.Spec.Apps {
		if app.Profile == "" {
			t.Errorf("Tenant %q apps[%d]: profile is empty", tenant.Name, i)
		}
	}
	switch tenant.Spec.DeletionPolicy {
	case v1alpha1.DeletionPolicyRetain, v1alpha1.DeletionPolicyDelete, "":
	default:
		t.Errorf("Tenant %q: unknown deletionPolicy %q", tenant.Name, tenant.Spec.DeletionPolicy)
	}
}

func validateIntegrationBinding(t *testing.T, ib *v1alpha1.IntegrationBinding) {
	t.Helper()
	if ib.Name == "" {
		t.Error("IntegrationBinding: metadata.name is empty")
	}
	if ib.Namespace == "" {
		t.Error("IntegrationBinding: metadata.namespace is empty")
	}
	if ib.Spec.Contract == "" {
		t.Error("IntegrationBinding: spec.contract is empty")
	}
	if ib.Spec.Provider.App == "" {
		t.Error("IntegrationBinding: spec.provider.app is empty")
	}
	if ib.Spec.Consumer.App == "" {
		t.Error("IntegrationBinding: spec.consumer.app is empty")
	}
	if ib.Spec.Auth != nil && ib.Spec.Auth.Method == "" {
		t.Error("IntegrationBinding: spec.auth is set but method is empty")
	}
}
