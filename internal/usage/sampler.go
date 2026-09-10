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

package usage

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/resourceplan"
)

// The sampler reads every tenant's ceiling and the pod metrics beneath it. The
// metrics.k8s.io grant is separate from core pods on purpose: it is served by
// metrics-server, an optional add-on, and a cluster without it should show the
// permission going unused rather than have the sampler fail closed.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=resourceplans,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=resourceplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

const (
	// TenantQuotaName is the ResourceQuota the tenant shell creates. Sampling
	// reads this one object and nothing else in the namespace: it is what the
	// cluster enforces, so it is what a bill can be defended with.
	TenantQuotaName = "tenant-quota"
)

// Sampler records each tenant's ceiling and consumption on a ticker.
type Sampler struct {
	Client client.Client
	// KernelNamespace holds the portal-shell-<tenant> Secrets whose
	// DATABASE_URL points at each tenant's own database.
	KernelNamespace string
	// Interval is how often every tenant is sampled.
	Interval time.Duration
	// Retention is how long samples are kept. Zero disables pruning; plan
	// events are never pruned regardless.
	Retention time.Duration
	// Actual, when set, adds a live-consumption series beside the committed
	// one. Nil is the supported state, not a degraded one — a cluster without
	// metrics-server still bills correctly, it just cannot say how much of the
	// plan is going unused.
	Actual ActualSource
}

// Start runs until ctx is cancelled. It implements manager.Runnable.
func (s *Sampler) Start(ctx context.Context) error {
	// Sample once at start-up rather than waiting out the first interval: an
	// operator restart is otherwise a hole in the series as long as the
	// interval, and restarts cluster around exactly the changes someone later
	// wants to read the series across.
	s.RunOnce(ctx)

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// NeedLeaderElection keeps one replica sampling. Two would write two rows per
// tick, and a naive sum over the interval would then double a tenant's usage.
func (s *Sampler) NeedLeaderElection() bool { return true }

// RunOnce samples every tenant once.
//
// One tenant's failure never stops the pass: a tenant whose database is
// unreachable loses its own samples, and letting that also stop the others
// would turn one broken tenant into a cluster-wide gap in the billing record.
func (s *Sampler) RunOnce(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("usage-sampler")

	var tenants gentianov1alpha1.TenantList
	if err := s.Client.List(ctx, &tenants); err != nil {
		logger.Error(err, "failed to list tenants")
		return
	}

	catalogue, err := resourceplan.Load(ctx, s.Client)
	if err != nil {
		// Plans are a label on the sample, not its substance. Without the
		// catalogue the quantities are still worth recording; what is lost is
		// the plan name, and a sample with quantities and no plan is more
		// useful than no sample.
		logger.Error(err, "failed to load resource plans; sampling without plan labels")
		catalogue = &resourceplan.Catalogue{}
	}

	observed := time.Now().UTC()
	for i := range tenants.Items {
		tenant := &tenants.Items[i]
		if !tenant.DeletionTimestamp.IsZero() {
			continue
		}
		if err := s.sampleTenant(ctx, tenant, catalogue, observed); err != nil {
			logger.Error(err, "failed to sample tenant", "tenant", tenant.Name)
		}
	}
}

func (s *Sampler) sampleTenant(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	catalogue *resourceplan.Catalogue,
	observed time.Time,
) error {
	nsName := tenant.NamespaceName()

	var quota corev1.ResourceQuota
	err := s.Client.Get(ctx, types.NamespacedName{Name: TenantQuotaName, Namespace: nsName}, &quota)
	switch {
	case apierrors.IsNotFound(err):
		// No quota is a real state — a tenant on no plan, running unbounded —
		// and recording it as an empty ceiling is how the series later shows
		// when a ceiling was first imposed.
	case err != nil:
		return fmt.Errorf("read %s in %s: %w", TenantQuotaName, nsName, err)
	}

	resolution := catalogue.Resolve(tenant)
	sample := Sample{
		ObservedAt: observed,
		Hard:       quantityMap(quota.Status.Hard),
		Used:       quantityMap(quota.Status.Used),
	}
	if resolution.Plan != nil {
		sample.Plan = resolution.Plan.Name
		sample.ProductSku = resolution.Plan.Spec.ProductSku
	}

	if s.Actual != nil {
		actual, err := s.Actual.NamespaceUsage(ctx, nsName)
		if err != nil {
			// Logged by the caller through the returned error only if nothing
			// else succeeded; here the committed figures are already in hand
			// and are the ones that matter, so an unavailable metrics API
			// costs the advisory series and not the sample.
			log.FromContext(ctx).V(1).Info("actual usage unavailable",
				"tenant", tenant.Name, "source", s.Actual.Name(), "error", err.Error())
		} else if actual != nil {
			sample.Actual = quantityMap(actual)
		}
	}

	store, err := s.storeFor(ctx, tenant.Name)
	if err != nil {
		return err
	}
	if err := store.EnsureSchema(ctx); err != nil {
		return err
	}
	if err := store.RecordSample(ctx, sample); err != nil {
		return err
	}

	if s.Retention > 0 {
		if _, err := store.Prune(ctx, observed.Add(-s.Retention)); err != nil {
			return fmt.Errorf("prune %s: %w", tenant.Name, err)
		}
	}
	return nil
}

// StoreFor opens the usage store for one tenant.
func (s *Sampler) storeFor(ctx context.Context, tenantName string) (*Store, error) {
	return StoreForTenant(ctx, s.Client, s.KernelNamespace, tenantName)
}

// StoreForTenant resolves a tenant's shell database from its portal-shell
// Secret and returns a usage store over it.
//
// Shared with the resources API, which reads the same history it writes: one
// resolver means the API can never end up reading a different database from
// the one the sampler is filling.
func StoreForTenant(
	ctx context.Context,
	c client.Client,
	kernelNamespace, tenantName string,
) (*Store, error) {
	secretName := fmt.Sprintf("portal-shell-%s", tenantName)
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: kernelNamespace}, &secret); err != nil {
		return nil, fmt.Errorf("read %s/%s: %w", kernelNamespace, secretName, err)
	}
	dsn := string(secret.Data["DATABASE_URL"])
	if dsn == "" {
		return nil, fmt.Errorf("%s/%s has no DATABASE_URL", kernelNamespace, secretName)
	}
	return NewStore(dsn), nil
}

func quantityMap(list corev1.ResourceList) map[string]string {
	if len(list) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(list))
	for name, q := range list {
		out[string(name)] = q.String()
	}
	return out
}
