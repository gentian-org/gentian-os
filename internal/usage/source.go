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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// metricsGroupVersion is the API metrics-server serves.
var metricsGroupVersion = schema.GroupVersion{Group: "metrics.k8s.io", Version: "v1beta1"}

// ActualSource reads live consumption for a namespace.
//
// The interface exists because the *history* is ours and the *reading* is not.
// Samples land in tenant_resource_samples whichever source produced them, so
// swapping metrics.k8s.io for a PromQL query later changes one implementation
// and discards nothing already collected. Without the seam, adopting Prometheus
// would mean either a second table or a gap in the series.
//
// Nothing here feeds billing. What a tenant owes is what the quota committed on
// their behalf, which comes from the API server and needs no source at all;
// live consumption answers a different question — whether the plan they are
// paying for is the plan they need.
type ActualSource interface {
	// NamespaceUsage returns live CPU and memory for a namespace, keyed by the
	// same ResourceQuota names the committed figures use so a caller can put
	// the two series on one axis. A nil map with a nil error means the source
	// has no reading yet, which is normal for a namespace whose pods started
	// seconds ago.
	NamespaceUsage(ctx context.Context, namespace string) (corev1.ResourceList, error)
	// Name identifies the source in logs and in the API response, so a chart
	// with no actual series can say why rather than just omitting it.
	Name() string
}

// MetricsAPISource reads metrics.k8s.io, served by metrics-server.
//
// Queried through the REST client rather than the typed metrics clientset to
// avoid a dependency on k8s.io/metrics for two fields; the PodMetrics shape
// below is the part of that API this uses and all of it that matters here.
type MetricsAPISource struct {
	client rest.Interface
}

// NewMetricsAPISource builds a source over the cluster's metrics API.
func NewMetricsAPISource(cfg *rest.Config) (*MetricsAPISource, error) {
	scoped := rest.CopyConfig(cfg)
	scoped.GroupVersion = &metricsGroupVersion
	scoped.APIPath = "/apis"
	// The kubernetes scheme knows nothing of metrics.k8s.io, which is fine:
	// every call here is DoRaw, so the serializer is only needed to satisfy
	// the client constructor and never decodes anything.
	scoped.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	c, err := rest.RESTClientFor(scoped)
	if err != nil {
		return nil, fmt.Errorf("build metrics API client: %w", err)
	}
	return &MetricsAPISource{client: c}, nil
}

func (m *MetricsAPISource) Name() string { return "metrics.k8s.io" }

// podMetricsList is the subset of metrics.k8s.io/v1beta1 PodMetricsList used here.
type podMetricsList struct {
	Items []struct {
		Containers []struct {
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// NamespaceUsage sums container usage across the namespace's pods.
//
// Summed rather than reported per pod: the ceiling being compared against is a
// namespace ResourceQuota, so the only comparable reading is the namespace
// total. Per-pod detail belongs to whatever observability stack the cluster
// later grows, not to a billing series.
func (m *MetricsAPISource) NamespaceUsage(ctx context.Context, namespace string) (corev1.ResourceList, error) {
	raw, err := m.client.Get().
		Resource("pods").
		Namespace(namespace).
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("read pod metrics for %s: %w", namespace, err)
	}
	var list podMetricsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode pod metrics for %s: %w", namespace, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}

	cpu := resource.NewQuantity(0, resource.DecimalSI)
	mem := resource.NewQuantity(0, resource.BinarySI)
	for _, pod := range list.Items {
		for _, ctr := range pod.Containers {
			if v, ok := ctr.Usage["cpu"]; ok {
				if q, err := resource.ParseQuantity(v); err == nil {
					cpu.Add(q)
				}
			}
			if v, ok := ctr.Usage["memory"]; ok {
				if q, err := resource.ParseQuantity(v); err == nil {
					mem.Add(q)
				}
			}
		}
	}

	// Keyed as limits.cpu / limits.memory to match ResourceListFromQuotas, so
	// the actual series lines up with the committed one on the same axis
	// without the reader having to know that one came from a quota and the
	// other from a metrics endpoint.
	//
	// CPU is re-scaled to milli before it leaves here. Summing kubelet readings
	// keeps their nanocore scale, so String() renders ten millicores as
	// "10269026n" — a valid quantity that every parser accepts and no human
	// reads. Milli is the unit the rest of Kubernetes states CPU in.
	return corev1.ResourceList{
		corev1.ResourceLimitsCPU:    *resource.NewMilliQuantity(cpu.MilliValue(), resource.DecimalSI),
		corev1.ResourceLimitsMemory: *mem,
	}, nil
}
