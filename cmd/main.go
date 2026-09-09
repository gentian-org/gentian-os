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

package main

import (
	"context"
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/applifecycle"
	"github.com/gentian-org/gentian-os/internal/controller"
	"github.com/gentian-org/gentian-os/internal/credentialmgr"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/usage"
	"github.com/gentian-org/gentian-os/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(discoveryv1.AddToScheme(scheme))
	utilruntime.Must(networkingv1.AddToScheme(scheme))
	utilruntime.Must(gentianov1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

// buildLogTailer gives the export loop a way to read a failed capture
// container's output. A cluster that will not hand out a clientset is not a
// reason to refuse to run: the operator starts without it and failures are
// merely less explicit.
func buildLogTailer(mgr ctrl.Manager) controller.PodLogTailer {
	cs, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "no clientset for reading capture logs; "+
			"capture failures will report that a Job failed but not why")
		return nil
	}
	return controller.ClientsetLogTailer{Clientset: cs}
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var enableWebhook bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Ensures only one active controller when scaled.")
	flag.BoolVar(&enableWebhook, "enable-webhook", false,
		"Enable the validating webhook server. Requires TLS certs in the webhook cert dir.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "gentianos.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	routingMode := os.Getenv("ROUTING_MODE")
	if routingMode == "" {
		routingMode = controller.RoutingModeGateway
	}
	setupLog.Info("edge routing mode", "routing_mode", routingMode)

	// Exec lets a profile's maintenance-mode and restore hooks run inside the
	// app's own pod. Optional: without it those fall back to scaling the app,
	// which still pauses writes, so a failure here must not stop the operator.
	// The nil stays a plain interface nil: assigning a typed-nil *PodExecer
	// would make every `Exec == nil` guard pass while calls panic.
	var appExecer controller.AppExecer
	if podExecer, execErr := controller.NewPodExecer(mgr.GetConfig()); execErr != nil {
		setupLog.Error(execErr, "pod exec unavailable; backup hooks will fall back to scaling")
	} else {
		appExecer = podExecer
	}

	tenantReconciler := &controller.TenantReconciler{
		Client:                   mgr.GetClient(),
		Exec:                     appExecer,
		Scheme:                   mgr.GetScheme(),
		Seeder:                   buildSeeder(),
		KernelDomain:             os.Getenv("KERNEL_DOMAIN"),
		TenancyMode:              os.Getenv("TENANCY_MODE"),
		MailServiceMode:          os.Getenv("MAIL_SERVICE_MODE"),
		TenantDNS01ClusterIssuer: os.Getenv("TENANT_DNS01_CLUSTER_ISSUER"),
		KernelRealm:              kernelRealmOrDefault(os.Getenv("KERNEL_REALM")),
		Ingress:                  buildEdgeIngress(),
		RoutingMode:              routingMode,
		CrossplaneOnly:           controller.EnvBool("TENANT_CROSSPLANE_ONLY"),
		CommerceEnabled:          controller.EnvBool("GENTIAN_COMMERCE_ENABLED"),
		CommerceAPIURL:           os.Getenv("GENTIAN_COMMERCE_API_URL"),
		CommerceAPIToken:         os.Getenv("GENTIAN_COMMERCE_API_TOKEN"),
	}
	if err := tenantReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tenant")
		os.Exit(1)
	}

	if controller.EnvBool("GENTIAN_COMMERCE_ENABLED") {
		interval := 1 * time.Hour
		if os.Getenv("METERING_INTERVAL") != "" {
			if parsed, err := time.ParseDuration(os.Getenv("METERING_INTERVAL")); err == nil {
				interval = parsed
			}
		}
		setupLog.Info("starting metering background worker", "interval", interval)
		if err := mgr.Add(&controller.MeteringWorker{
			Reconciler: tenantReconciler,
			Interval:   interval,
		}); err != nil {
			setupLog.Error(err, "unable to add metering worker to manager")
			os.Exit(1)
		}
	}

	// Usage sampling: the tenant ceiling and what is committed under it,
	// recorded on a ticker so "current use" has a history behind it and a
	// month resolves to something invoiceable. Off by a single env var, since
	// a cluster without the per-tenant shell databases has nowhere to write.
	if os.Getenv("USAGE_SAMPLER_ENABLED") != "false" {
		sampler := &usage.Sampler{
			Client:          mgr.GetClient(),
			KernelNamespace: envOrDefault("KERNEL_NAMESPACE", meta.KernelNamespace),
			Interval:        envDuration("USAGE_SAMPLE_INTERVAL", 15*time.Minute),
			Retention:       envDuration("USAGE_RETENTION", 400*24*time.Hour),
		}
		// The live series is optional and its absence is not an error: a
		// cluster with no metrics-server still records the figures a plan is
		// chosen and billed on, and only loses the answer to "is this tenant
		// using what they pay for".
		if controller.EnvBool("METRICS_SERVER_ENABLED") {
			src, err := usage.NewMetricsAPISource(mgr.GetConfig())
			if err != nil {
				setupLog.Error(err, "unable to build the metrics usage source; sampling committed usage only")
			} else {
				sampler.Actual = src
			}
		}
		if err := mgr.Add(sampler); err != nil {
			setupLog.Error(err, "unable to add the usage sampler to manager")
			os.Exit(1)
		}
		setupLog.Info("usage sampler enabled",
			"interval", sampler.Interval, "retention", sampler.Retention,
			"actualSource", sampler.Actual != nil)
	}

	if err := (&controller.KeycloakPlatformReconciler{
		Client:       mgr.GetClient(),
		KernelDomain: os.Getenv("KERNEL_DOMAIN"),
		TenancyMode:  os.Getenv("TENANCY_MODE"),
		KernelRealm:  kernelRealmOrDefault(os.Getenv("KERNEL_REALM")),
		RoutingMode:  routingMode,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KeycloakPlatform")
		os.Exit(1)
	}

	if err := (&controller.GatewayPlatformReconciler{
		Client:       mgr.GetClient(),
		KernelDomain: os.Getenv("KERNEL_DOMAIN"),
		TenancyMode:  os.Getenv("TENANCY_MODE"),
		RoutingMode:  routingMode,
		Ingress:      buildEdgeIngress(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GatewayPlatform")
		os.Exit(1)
	}

	if err := (&controller.AppStoreReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AppStore")
		os.Exit(1)
	}

	if err := (&controller.CustomizationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Customization")
		os.Exit(1)
	}

	openfgaURL := os.Getenv("OPENFGA_API_URL")
	if openfgaURL == "" {
		openfgaURL = "http://gentian-openfga.platform-kernel.svc.cluster.local:8080"
	}
	if err := (&controller.AuthzBridgeReconciler{
		Client:       mgr.GetClient(),
		KernelRealm:  kernelRealmOrDefault(os.Getenv("KERNEL_REALM")),
		OpenFGAURL:   openfgaURL,
		OpenFGAToken: os.Getenv("OPENFGA_API_TOKEN"),
		Enabled:      controller.AuthzBridgeEnabled(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AuthzBridge")
		os.Exit(1)
	}
	if controller.AuthzBridgeEnabled() {
		setupLog.Info("authz bridge enabled", "openfga_url", openfgaURL)
	}

	if err := (&controller.PlatformSecurityPolicyReconciler{
		Client:            mgr.GetClient(),
		OperatorNamespace: "gentian-system",
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PlatformSecurityPolicy")
		os.Exit(1)
	}

	if err := (&controller.AppGrantReconciler{
		Client:       mgr.GetClient(),
		OpenFGAURL:   openfgaURL,
		OpenFGAToken: os.Getenv("OPENFGA_API_TOKEN"),
		Enabled:      controller.AuthzBridgeEnabled(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AppGrant")
		os.Exit(1)
	}

	if err := (&controller.IntegrationBindingReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Seeder: buildSeeder(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "IntegrationBinding")
		os.Exit(1)
	}

	tenantExportReconciler := &controller.TenantExportReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Reconciler: tenantReconciler,
		// The API reader, not the cached client: appVolumes explains why a
		// cached PVC read is how an export comes to hold an app offline
		// indefinitely with nothing in the log.
		VolumeReader: mgr.GetAPIReader(),
		// Logs are a subresource served as a stream, so they need a clientset
		// rather than the manager's client. Nil is tolerated by the reconciler;
		// a failure then reports that a Job failed and not why.
		LogTailer: buildLogTailer(mgr),
	}
	if err := tenantExportReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TenantExport")
		os.Exit(1)
	}

	if err := (&controller.TenantRestoreReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Reconciler: tenantExportReconciler,
		Tenant:     tenantReconciler,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TenantRestore")
		os.Exit(1)
	}

	if err := (&controller.TenantExportScheduleReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TenantExportSchedule")
		os.Exit(1)
	}

	if err := (&controller.BackupPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupPolicy")
		os.Exit(1)
	}

	if enableWebhook {
		(&webhook.TenantValidator{
			Client:       mgr.GetClient(),
			TenancyMode:  os.Getenv("TENANCY_MODE"),
			KernelDomain: os.Getenv("KERNEL_DOMAIN"),
			// Default on. A cluster that has not proven its administrator can
			// write credentials is one where the recovery from a broken write
			// path is still cheap, and admitting tenants is what makes it
			// expensive — so the safe default is the one that keeps the exit open.
			GateOnHandover:    os.Getenv("HANDOVER_GATE_TENANTS") != "false",
			HandoverNamespace: envOrDefault("HANDOVER_NAMESPACE", envOrDefault("OPERATOR_NAMESPACE", "gentian-system")),
		}).SetupWithManager(mgr)

		(&webhook.AppProfileValidator{
			Client: mgr.GetClient(),
		}).SetupWithManager(mgr)
	}

	if os.Getenv("APP_LIFECYCLE_ENABLED") != "false" {
		lifecycle, err := applifecycle.NewRunnableFromEnv(mgr)
		if err != nil {
			setupLog.Error(err, "unable to create app lifecycle server")
			os.Exit(1)
		}
		if err := mgr.Add(lifecycle); err != nil {
			setupLog.Error(err, "unable to add app lifecycle server")
			os.Exit(1)
		}
		setupLog.Info("app lifecycle API enabled", "addr", lifecycle.Server.Addr)
	}

	// Credential Manager — a view over the CredentialRequirement catalogue and
	// ESO's satisfaction status, plus a write path that writes as the CALLER.
	// It rides this manager rather than being a second Deployment, and holds no
	// OpenBao token of its own: every write exchanges the user's OIDC token.
	if os.Getenv("CREDENTIAL_MANAGER_ENABLED") == "true" {
		credmgr, err := credentialmgr.NewRunnableFromEnv(mgr, credentialmgr.NewEndpointValidator())
		if err != nil {
			setupLog.Error(err, "unable to create the credential manager")
			os.Exit(1)
		}
		if err := mgr.Add(credmgr); err != nil {
			setupLog.Error(err, "unable to add the credential manager")
			os.Exit(1)
		}
		setupLog.Info("credential manager API enabled", "addr", credmgr.Addr)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// kernelRealmOrDefault returns realm if non-empty, otherwise "kernel".
func kernelRealmOrDefault(realm string) string {
	if realm == "" {
		return "kernel"
	}
	return realm
}

// buildSeeder constructs a secrets.Seeder backed by an OpenBao KV v2 client
// authenticated via the Kubernetes auth method. Required env vars:
//
//	BAO_ADDR  — OpenBao address (e.g. http://openbao.openbao.svc.cluster.local:8200)
//	BAO_ROLE  — Kubernetes auth role name (default: gentian-os-operator)
//
// The platform master password is read once at startup from
// secrets.MasterPasswordPath and fed into an HKDF-SHA256 deriver so that
// every (tenant, app, category) credential the operator generates is fully
// deterministic — uninstalling and reinstalling an app yields the same
// credentials.
//
// Returns nil (with a warning) when BAO_ADDR is unset — in that mode
// reconcilers skip the seeding step and behave as they did pre-Inc 21a.
// This keeps envtest suites running without an OpenBao double and lets
// early cluster bring-up proceed in stages.
func buildSeeder() *secrets.Seeder {
	baoAddr := os.Getenv("BAO_ADDR")
	if baoAddr == "" {
		setupLog.Info("secret seeder disabled: set BAO_ADDR to enable")
		return nil
	}
	role := os.Getenv("BAO_ROLE")
	if role == "" {
		role = "gentian-os-operator"
	}
	kv := secrets.NewKVClient(baoAddr, role, "")

	// Fetch the master password once at startup. If it isn't there yet the
	// seeder still functions (falls back to crypto/rand), but credentials
	// will not be deterministic — log loudly so the operator notices the
	// missing bootstrap step.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	master, err := kv.Get(ctx, secrets.MasterPasswordPath)
	var deriver *secrets.Deriver
	switch {
	case err != nil:
		setupLog.Error(err, "failed to read master password from OpenBao; credentials will be random",
			"path", secrets.MasterPasswordPath)
	case master["value"] == "":
		setupLog.Info("master password not found at expected path; credentials will be random",
			"path", secrets.MasterPasswordPath)
	default:
		deriver = secrets.NewDeriver(master["value"], master["salt"])
	}

	setupLog.Info("secret seeder enabled",
		"bao_addr", baoAddr, "bao_role", role, "deterministic", deriver != nil)
	return secrets.NewSeeder(kv, deriver)
}

// buildEdgeIngress constructs how traffic reaches this cluster, by NAME.
//
// The name is the key in kernel/platforms.yaml's edgeIngress table, which is
// also what decides the credential the installer asks for. One string, two
// consumers, so they cannot disagree. Which implementation that name maps to
// lives in the registry (internal/controller/edge_registry.go), not here, so
// adding an ingress never touches this function.
//
// There is no DNS half. external-dns writes this cluster's records -- every
// provider in kernel/platforms.yaml, from the HTTPRoutes this operator writes
// and from DNSEndpoint CRs for hostnames a tunnelled Gateway cannot supply an
// address for. What an ingress contributes is the TARGET those records point
// at. See internal/controller/edge.go.
//
//	EDGE_INGRESS            – which one; defaults from NETWORK_MODE so an
//	                          existing cluster needs no new value
//	CF_TUNNEL_TOKEN         – the ingress credential, falling back to
//	                          CLOUDFLARE_API_TOKEN for single-token clusters
//	CLOUDFLARE_TUNNEL_CNAME – the tunnel to point at
//	CLOUDFLARE_ZONE_ID      – only to resolve the account when the next is unset
//	CLOUDFLARE_ACCOUNT_ID   – supplied, the ingress never reads the zone
func buildEdgeIngress() controller.EdgeIngress {
	name := os.Getenv("EDGE_INGRESS")
	if name == "" {
		// Follows networkMode, the only thing that decides it today. A
		// static-ip cluster routes by LoadBalancer address and programs no
		// ingress; anything else is the tunnel.
		if os.Getenv("NETWORK_MODE") == "static-ip" {
			name = "none"
		} else {
			name = "cf-tunnel"
		}
	}

	token := os.Getenv("CF_TUNNEL_TOKEN")
	separate := token != ""
	if token == "" {
		token = os.Getenv("CLOUDFLARE_API_TOKEN")
	}

	ing, err := controller.BuildEdgeIngress(name, controller.EdgeIngressConfig{
		Token:   token,
		Target:  os.Getenv("CLOUDFLARE_TUNNEL_CNAME"),
		Zone:    os.Getenv("CLOUDFLARE_ZONE_ID"),
		Account: os.Getenv("CLOUDFLARE_ACCOUNT_ID"),
	})
	if err != nil {
		// Named but unbuildable is a configuration error, and continuing would
		// route nothing while looking like a cluster that programs no ingress.
		setupLog.Error(err, "edge ingress is configured but could not be built", "edge_ingress", name)
		return nil
	}
	if ing == nil {
		setupLog.Info("no edge ingress; traffic is expected to reach the gateway directly",
			"edge_ingress", name)
		return nil
	}
	setupLog.Info("edge ingress enabled",
		"edge_ingress", name,
		"separate_token", separate,
		"account_id_supplied", os.Getenv("CLOUDFLARE_ACCOUNT_ID") != "")
	return ing
}

// envOrDefault reads an environment variable, falling back when it is unset or
// empty. Empty and unset are treated alike deliberately: a Helm value rendered
// to "" is how an unset chart value reaches a container, and treating that as a
// deliberate empty string means a default that silently stops applying.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDuration reads a Go duration from the environment, falling back when it is
// unset or unparseable.
//
// A malformed value falls back rather than exiting: the sampler's interval is
// an operational preference, and refusing to start the whole operator over a
// mistyped "15min" would turn a cosmetic error into an outage.
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parsed, err := time.ParseDuration(v)
	if err != nil || parsed <= 0 {
		setupLog.Info("ignoring unparseable duration; using the default",
			"key", key, "value", v, "default", def)
		return def
	}
	return parsed
}
