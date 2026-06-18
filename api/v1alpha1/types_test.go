package v1alpha1_test

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ----- AppProfile tests -----

func TestAppProfile_DeepCopy(t *testing.T) {
	qty := resource.MustParse("5Gi")

	original := &v1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "openproject"},
		Spec: v1alpha1.AppProfileSpec{
			DisplayName:      "OpenProject",
			DeploymentMethod: v1alpha1.DeploymentMethodArgoCD,
			Chart: v1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       "openproject",
				Version:    "14.2.0",
			},
			KernelRequirements: &v1alpha1.KernelRequirements{
				Identity: &v1alpha1.IdentityRequirement{
					OIDC: &v1alpha1.OIDCClientSpec{ClientID: "test-client"},
					LDAP: &v1alpha1.LDAPRequirement{Sync: true, Interval: "1h"},
				},
				Database: &v1alpha1.DatabaseRequirement{
					Engine:            v1alpha1.DatabaseEnginePostgreSQL,
					DatabasePerTenant: true,
				},
				Storage: &v1alpha1.StorageRequirement{
					S3: &v1alpha1.S3Requirement{BucketPerTenant: true},
				},
				Cache: &v1alpha1.CacheRequirement{Engine: v1alpha1.CacheEngineMemcached},
				Mail: &v1alpha1.MailRequirement{
					SMTP: &v1alpha1.SMTPRequirement{Auth: "cram-md5", Port: 587},
				},
				MCP: &v1alpha1.MCPRequirement{Enabled: true, Endpoint: "/mcp", Auth: "oidc"},
			},
			ValueMapping: &v1alpha1.ValueMapping{
				OIDC: &v1alpha1.OIDCValueMapping{
					IssuerKey:       "oidc.issuer",
					ClientIDKey:     "oidc.clientId",
					ClientSecretKey: "oidc.clientSecret",
				},
				Database: &v1alpha1.DatabaseValueMapping{
					HostKey:     "database.host",
					NameKey:     "database.name",
					UserKey:     "database.user",
					PasswordKey: "database.password",
				},
			},
			AppSecrets: []v1alpha1.AppSecret{
				{Name: "admin_password", ValuePath: "openproject.adminPassword"},
			},
		},
	}
	_ = qty

	copy := original.DeepCopy()

	if copy.Name != original.Name {
		t.Errorf("expected Name %q, got %q", original.Name, copy.Name)
	}
	if copy.Spec.Chart.Version != original.Spec.Chart.Version {
		t.Errorf("expected chart version %q, got %q", original.Spec.Chart.Version, copy.Spec.Chart.Version)
	}
	if copy.Spec.KernelRequirements.Identity.LDAP.Interval != "1h" {
		t.Errorf("expected LDAP interval 1h, got %q", copy.Spec.KernelRequirements.Identity.LDAP.Interval)
	}
	if len(copy.Spec.AppSecrets) != 1 {
		t.Fatalf("expected 1 appSecret, got %d", len(copy.Spec.AppSecrets))
	}
	if copy.Spec.AppSecrets[0].Name != "admin_password" {
		t.Errorf("expected appSecret name admin_password, got %q", copy.Spec.AppSecrets[0].Name)
	}

	// Mutation of copy must not affect original
	copy.Spec.AppSecrets[0].Name = "mutated"
	if original.Spec.AppSecrets[0].Name == "mutated" {
		t.Error("DeepCopy did not produce independent AppSecrets slice")
	}
}

func TestAppProfile_DefaultDeploymentMethod(t *testing.T) {
	ap := &v1alpha1.AppProfile{
		Spec: v1alpha1.AppProfileSpec{
			DeploymentMethod: v1alpha1.DeploymentMethodArgoCD,
		},
	}
	if ap.Spec.DeploymentMethod != v1alpha1.DeploymentMethodArgoCD {
		t.Errorf("expected default argocd, got %q", ap.Spec.DeploymentMethod)
	}
}

func TestAppProfile_ExtraValues_RoundTrip(t *testing.T) {
	raw := `{"smtp":{"port":587},"someNested":{"key":"value"}}`
	ap := &v1alpha1.AppProfile{
		Spec: v1alpha1.AppProfileSpec{
			DisplayName: "Test",
			Chart:       v1alpha1.ChartRef{Repository: "oci://r", Name: "n", Version: "1.0.0"},
			ExtraValues: &runtime.RawExtension{Raw: []byte(raw)},
		},
	}

	// Round-trip through JSON
	data, err := json.Marshal(ap)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var restored v1alpha1.AppProfile
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	copy := ap.DeepCopy()
	if copy.Spec.ExtraValues == nil {
		t.Error("DeepCopy lost ExtraValues")
	}
	if string(copy.Spec.ExtraValues.Raw) != raw {
		t.Errorf("ExtraValues mismatch: got %q", string(copy.Spec.ExtraValues.Raw))
	}
}

// ----- Tenant tests -----

func TestTenant_DeepCopy(t *testing.T) {
	replicas := int32(2)
	storage := resource.MustParse("100Gi")
	cpu := resource.MustParse("8")
	memory := resource.MustParse("16Gi")
	quotaPerUser := resource.MustParse("5Gi")

	original := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "gtn-demo"},
		Spec: v1alpha1.TenantSpec{
			DisplayName:    "GTN Demo",
			Domain:         "gtn-demo.example.com",
			AdminEmail:     "admin@gtn-demo.example.com",
			DeletionPolicy: v1alpha1.DeletionPolicyRetain,
			Isolation: &v1alpha1.TenantIsolation{
				Mode:           v1alpha1.IsolationModeNamespace,
				KeycloakRealm:  "gtn-demo",
				DatabasePrefix: "gtn_",
				S3Prefix:       "gtn-demo-",
				LDAPOu:         "ou=gtn-demo",
			},
			Mail: &v1alpha1.TenantMail{
				Mode:         v1alpha1.MailModeSelfhosted,
				Domain:       "gtn-demo.example.com",
				QuotaPerUser: &quotaPerUser,
				RateLimit:    "100/h",
			},
			Quotas: &v1alpha1.TenantQuotas{
				MaxApps: 20,
				Storage: &storage,
				CPU:     &cpu,
				Memory:  &memory,
			},
			Apps: []v1alpha1.TenantApp{
				{Profile: "nextcloud"},
				{Profile: "ox-appsuite"},
				{Profile: "openproject", Config: &v1alpha1.TenantAppConfig{Replicas: &replicas}},
			},
		},
	}

	copy := original.DeepCopy()

	if copy.Name != "gtn-demo" {
		t.Errorf("expected name gtn-demo, got %q", copy.Name)
	}
	if len(copy.Spec.Apps) != 3 {
		t.Fatalf("expected 3 apps, got %d", len(copy.Spec.Apps))
	}
	if copy.Spec.Apps[2].Config.Replicas == nil || *copy.Spec.Apps[2].Config.Replicas != 2 {
		t.Error("replica count not preserved in DeepCopy")
	}

	// Mutation independence
	*copy.Spec.Apps[2].Config.Replicas = 99
	if *original.Spec.Apps[2].Config.Replicas == 99 {
		t.Error("DeepCopy did not produce independent TenantApp slice")
	}

	// Quota quantity independence
	copy.Spec.Quotas.Storage = nil
	if original.Spec.Quotas.Storage == nil {
		t.Error("DeepCopy Quotas.Storage is not independent")
	}
}

func TestTenant_DeletionPolicyValues(t *testing.T) {
	cases := []struct {
		policy v1alpha1.DeletionPolicy
		valid  bool
	}{
		{v1alpha1.DeletionPolicyRetain, true},
		{v1alpha1.DeletionPolicyDelete, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			tenant := &v1alpha1.Tenant{
				Spec: v1alpha1.TenantSpec{
					DisplayName:    "T",
					Domain:         "t.example.com",
					AdminEmail:     "a@t.example.com",
					DeletionPolicy: tc.policy,
				},
			}
			if tenant.Spec.DeletionPolicy != tc.policy {
				t.Errorf("expected %q, got %q", tc.policy, tenant.Spec.DeletionPolicy)
			}
		})
	}
}

func TestTenant_StatusDeepCopy(t *testing.T) {
	now := metav1.Now()
	original := &v1alpha1.Tenant{
		Status: v1alpha1.TenantStatus{
			Phase:           v1alpha1.TenantPhaseReady,
			Namespace:       "tenant-acme",
			ProvisionedApps: []string{"nextcloud", "ox-appsuite"},
			Conditions: []metav1.Condition{
				{
					Type:               "NamespaceReady",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
					Reason:             "Created",
					Message:            "namespace created",
				},
			},
		},
	}

	copy := original.DeepCopy()

	if len(copy.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(copy.Status.Conditions))
	}
	if len(copy.Status.ProvisionedApps) != 2 {
		t.Fatalf("expected 2 provisioned apps, got %d", len(copy.Status.ProvisionedApps))
	}

	// Mutation independence
	copy.Status.ProvisionedApps[0] = "mutated"
	if original.Status.ProvisionedApps[0] == "mutated" {
		t.Error("DeepCopy did not produce independent ProvisionedApps slice")
	}
}

// ----- IntegrationBinding tests -----

func TestIntegrationBinding_DeepCopy(t *testing.T) {
	now := metav1.Now()
	original := &v1alpha1.IntegrationBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gtn-demo-filepicker",
			Namespace: "tenant-gtn-demo",
		},
		Spec: v1alpha1.IntegrationBindingSpec{
			Contract:     "filepicker",
			Provider:     v1alpha1.AppEndpoint{App: "nextcloud", Namespace: "tenant-gtn-demo"},
			Consumer:     v1alpha1.AppEndpoint{App: "ox-appsuite", Namespace: "tenant-gtn-demo"},
			Capabilities: []string{"webdav:read", "webdav:write", "ocs:shares"},
			Auth: &v1alpha1.BindingAuth{
				Method:    "oidc-token-exchange",
				VaultPath: "gentianos/tenants/gtn-demo/contracts/filepicker",
			},
		},
		Status: v1alpha1.IntegrationBindingStatus{
			State: v1alpha1.IntegrationBindingStateReady,
			Conditions: []metav1.Condition{
				{
					Type:               "CredentialsValid",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
					Reason:             "Provisioned",
				},
			},
			SecretRef:    &v1alpha1.LocalSecretRef{Name: "contract-filepicker"},
			LastRotation: &now,
		},
	}

	copy := original.DeepCopy()

	if copy.Name != original.Name {
		t.Errorf("name mismatch: got %q", copy.Name)
	}
	if copy.Spec.Auth.VaultPath != original.Spec.Auth.VaultPath {
		t.Errorf("vault path mismatch: got %q", copy.Spec.Auth.VaultPath)
	}
	if copy.Status.SecretRef.Name != "contract-filepicker" {
		t.Errorf("secretRef name mismatch: got %q", copy.Status.SecretRef.Name)
	}

	// Mutation independence — capabilities
	copy.Spec.Capabilities[0] = "mutated"
	if original.Spec.Capabilities[0] == "mutated" {
		t.Error("DeepCopy did not produce independent Capabilities slice")
	}

	// Mutation independence — auth
	copy.Spec.Auth.VaultPath = "mutated"
	if original.Spec.Auth.VaultPath == "mutated" {
		t.Error("DeepCopy did not produce independent Auth struct")
	}
}

func TestIntegrationBinding_StateValues(t *testing.T) {
	states := []v1alpha1.IntegrationBindingState{
		v1alpha1.IntegrationBindingStatePending,
		v1alpha1.IntegrationBindingStateReady,
		v1alpha1.IntegrationBindingStateFailed,
	}
	for _, s := range states {
		ib := &v1alpha1.IntegrationBinding{
			Status: v1alpha1.IntegrationBindingStatus{State: s},
		}
		if ib.Status.State != s {
			t.Errorf("expected state %q, got %q", s, ib.Status.State)
		}
	}
}

// ----- List type DeepCopy tests -----

func TestAppProfileList_DeepCopy(t *testing.T) {
	list := &v1alpha1.AppProfileList{
		Items: []v1alpha1.AppProfile{
			{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ox-appsuite"}},
		},
	}
	copy := list.DeepCopy()
	if len(copy.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(copy.Items))
	}
	copy.Items[0].Name = "mutated"
	if list.Items[0].Name == "mutated" {
		t.Error("AppProfileList DeepCopy not independent")
	}
}

func TestTenantList_DeepCopy(t *testing.T) {
	list := &v1alpha1.TenantList{
		Items: []v1alpha1.Tenant{
			{ObjectMeta: metav1.ObjectMeta{Name: "gtn-demo"}},
		},
	}
	copy := list.DeepCopy()
	copy.Items[0].Name = "mutated"
	if list.Items[0].Name == "mutated" {
		t.Error("TenantList DeepCopy not independent")
	}
}

func TestIntegrationBindingList_DeepCopy(t *testing.T) {
	list := &v1alpha1.IntegrationBindingList{
		Items: []v1alpha1.IntegrationBinding{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
		},
	}
	copy := list.DeepCopy()
	copy.Items[0].Name = "mutated"
	if list.Items[0].Name == "mutated" {
		t.Error("IntegrationBindingList DeepCopy not independent")
	}
}

// ----- Scheme registration test -----

func TestSchemeRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	types := []runtime.Object{
		&v1alpha1.AppProfile{},
		&v1alpha1.AppProfileList{},
		&v1alpha1.AppProduct{},
		&v1alpha1.AppProductList{},
		&v1alpha1.Tenant{},
		&v1alpha1.TenantList{},
		&v1alpha1.IntegrationBinding{},
		&v1alpha1.IntegrationBindingList{},
	}
	for _, obj := range types {
		gvks, _, err := scheme.ObjectKinds(obj)
		if err != nil {
			t.Errorf("type %T not registered: %v", obj, err)
			continue
		}
		if len(gvks) == 0 {
			t.Errorf("no GVKs found for %T", obj)
		}
		for _, gvk := range gvks {
			if gvk.Group != "gentianos.io" {
				t.Errorf("expected group gentianos.io for %T, got %q", obj, gvk.Group)
			}
			if gvk.Version != "v1alpha1" {
				t.Errorf("expected version v1alpha1 for %T, got %q", obj, gvk.Version)
			}
		}
	}
}
