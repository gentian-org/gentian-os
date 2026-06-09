/*
Copyright 2026 The Gentian Authors.

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
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// tenantsTotal is the current number of Tenant CRs tracked by the orchestrator.
	tenantsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gentianos_tenants_total",
		Help: "Total number of Tenant CRs managed by the orchestrator.",
	})

	// tenantAppsTotal tracks the total requested apps across all tenants (labelled by tenant).
	tenantAppsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gentianos_tenant_apps_total",
		Help: "Total number of requested apps per tenant.",
	}, []string{"tenant"})

	// provisioningDuration observes how long a full reconcile / provisioning pass takes.
	provisioningDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gentianos_provisioning_duration_seconds",
		Help:    "Duration in seconds of a tenant provisioning reconcile pass.",
		Buckets: prometheus.DefBuckets,
	}, []string{"tenant"})

	// reconcileErrors counts reconciliation errors, labelled by controller type.
	reconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gentianos_reconcile_errors_total",
		Help: "Total number of reconcile errors by controller type.",
	}, []string{"controller"})

	// credentialsAge tracks the age in seconds of provisioned credentials per tenant.
	credentialsAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gentianos_credentials_age_seconds",
		Help: "Age in seconds of last-provisioned credentials per tenant.",
	}, []string{"tenant"})

	// integrationBindingsStatus tracks IntegrationBinding health by contract type.
	integrationBindingsStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gentianos_integration_bindings_status",
		Help: "Status of IntegrationBindings per contract type (1=bound, 0=unbound).",
	}, []string{"contract", "tenant"})

	// externalSecretsSync tracks ESO ExternalSecret sync health per tenant.
	externalSecretsSync = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gentianos_externalsecrets_sync_status",
		Help: "ExternalSecret sync status per tenant (1=synced, 0=error).",
	}, []string{"tenant"})

	// operatorCRReady counts operator CRs that have reached Ready state.
	operatorCRReady = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gentianos_operator_cr_ready_total",
		Help: "Total number of operator CRs that have reached Ready state.",
	}, []string{"kind", "tenant"})

	// operatorCRFailed counts operator CRs that failed to reach Ready state.
	operatorCRFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gentianos_operator_cr_failed_total",
		Help: "Total number of operator CRs that failed to reach Ready state.",
	}, []string{"kind", "tenant"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		tenantsTotal,
		tenantAppsTotal,
		provisioningDuration,
		reconcileErrors,
		credentialsAge,
		integrationBindingsStatus,
		externalSecretsSync,
		operatorCRReady,
		operatorCRFailed,
	)
}
