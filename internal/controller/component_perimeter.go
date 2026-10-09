/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// The tenant's DMZ, and what stands in it (AD-6, AD-9).
//
// A perimeter surface is a capability a profile DECLARES and a perimeter
// approver ENABLES, per install, with an expiry. Declaring one publishes
// nothing: until somebody with can_expose says so, and says until when, there
// is no namespace, no proxy and no route.
//
// When they do, what appears is deliberately small. One proxy per enabled
// exposure, in tenant-<t>-dmz, forwarding only the prefixes the profile
// declared to the component's Service in tenant-<t>. The DMZ holds no data,
// no credential of the tenant's and no session: if it is taken, what the
// attacker has is the ability to make requests that anybody on the internet
// could already make to the same URLs.
//
// The namespace is separate from tenant-<t> for that reason. A pod on the
// public internet with no session in front of it does not belong in the same
// blast radius as the tenant's application and its database.

// The publishing proxy is a Deployment this operator creates and removes, so
// it has to be allowed to: without create the proxy never starts, the route
// is never written, and a published host answers 404 from the Gateway with
// the listener in place and nothing behind it. The markers are a
// free-floating block: controller-gen ignores one that is part of a
// declaration's doc comment.
//
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// perimeterEnablement is one live enablement paired with the profile entry it
// enables. Both are needed and they come from different people: the profile
// says which paths this surface is, the enablement says it may be published,
// where, and until when.
type perimeterEnablement struct {
	spec *gentianov1alpha1.ExposureSpec
	on   *gentianov1alpha1.ExposureEnablement
	host string
	// website marks a tenant's surface on the cluster's main address: its
	// proxy refuses the platform's paths there (main_address.go).
	website bool
	// stepsBack marks the platform's own surface on the bare domain while a
	// tenant's website holds it: routed for the platform's paths only.
	stepsBack bool
}

// perimeterMainAddress is what the main address means for one component's
// perimeter, decided by mainAddressHolder before anything is published.
type perimeterMainAddress struct {
	// entry is this component's entry that holds the main address, and host
	// the address; both empty when none does.
	entry, host string
	// held says a tenant's website holds the main address, whoever's it is.
	held bool
}

// livePerimeterExposures are the enablements in force right now.
//
// An expired enablement is treated as absent, which is what makes the expiry
// mean anything: the proxy goes, the route goes, and the link stops answering
// without anybody having to remember. The entry stays in git as the record
// that it was once published.
func livePerimeterExposures(
	comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile,
	zone edgeZone, now time.Time, main perimeterMainAddress,
) []perimeterEnablement {
	byName := map[string]*gentianov1alpha1.ExposureSpec{}
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.Surface == gentianov1alpha1.SurfacePerimeter {
			byName[e.Name] = e
		}
	}
	var out []perimeterEnablement
	for i := range comp.Spec.Exposures {
		on := &comp.Spec.Exposures[i]
		spec, ok := byName[on.ExposureName]
		if !ok {
			// Enabled against an entry the profile no longer declares, or
			// never did. Publishing it would be publishing something nobody
			// wrote down.
			continue
		}
		if !spec.ApprovedAs(on.Kind) {
			// Approved as something else than the entry declares now: as a
			// plain public address, and the profile has since asked to have
			// the caller's credential passed on, or the other way round; or
			// in a mode the platform does not serve. What was approved is
			// not what would be published, so nothing is, until the entry
			// is approved as what it is.
			continue
		}
		if on.ExpiresAt != nil && !now.Before(on.ExpiresAt.Time) {
			continue
		}
		// Where it answers, by the rule the director shows an approver the
		// address with (addresses.PerimeterHost): the main address takes the
		// profile's author and the approver both saying so.
		host, website := addresses.PerimeterHost(zone.shared(), comp.Name, spec, on.Apex, main.entry, main.host)
		if host == "" {
			// An entry with nowhere to answer in this zone: the bare domain,
			// asked for by a tenant that does not hold it.
			continue
		}
		out = append(out, perimeterEnablement{
			spec: spec, on: on, host: host, website: website,
			stepsBack: zone.kernel && spec.Apex && main.held,
		})
	}
	return out
}

// perimeterName is the object name for one published exposure, stable and
// within 63 characters however long the component and entry names are.
func perimeterName(comp *gentianov1alpha1.Component, entry string) string {
	name := "perimeter-" + comp.Name + "-" + entry
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(comp.Name + "/" + entry))
	return "perimeter-" + hex.EncodeToString(sum[:])[:16]
}

// perimeterProxyAppName labels every object of a publishing proxy.
const perimeterProxyAppName = "perimeter-proxy"

func perimeterLabels(comp *gentianov1alpha1.Component, entry string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": managedByValue,
		"app.kubernetes.io/name":       perimeterProxyAppName,
		"gentianos.io/component":       comp.Name,
		"gentianos.io/exposure":        entry,
	}
}

// ensurePerimeter brings the tenant's DMZ to what the enablements say, and
// removes what they no longer say.
func (r *ComponentReconciler) ensurePerimeter(
	ctx context.Context, comp *gentianov1alpha1.Component,
	profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant, zone edgeZone,
) (int, error) {
	main, err := r.mainAddressFor(ctx, comp, profile, tenant)
	if err != nil {
		return 0, fmt.Errorf("main address: %w", err)
	}
	live := livePerimeterExposures(comp, profile, zone, time.Now(), main)
	dmz := layout.TenantDMZ(tenant.Name)

	if err := r.prunePerimeter(ctx, comp, dmz, live); err != nil {
		return 0, err
	}
	if len(live) == 0 {
		return 0, nil
	}
	if err := r.ensureDMZNamespace(ctx, tenant, dmz); err != nil {
		return 0, fmt.Errorf("dmz namespace: %w", err)
	}
	for i := range live {
		if err := r.ensurePerimeterProxy(ctx, comp, &live[i], dmz); err != nil {
			return 0, fmt.Errorf("perimeter %s: %w", live[i].spec.Name, err)
		}
	}
	return len(live), nil
}

// ensureDMZNamespace makes the tenant's perimeter namespace, labelled by tier
// so the policies that select on tier apply to it (AD-7).
func (r *ComponentReconciler) ensureDMZNamespace(ctx context.Context, tenant *gentianov1alpha1.Tenant, dmz string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: dmz,
		Labels: map[string]string{
			"gentianos.io/tier":            string(layout.TierTenantDMZ),
			"gentianos.io/tenant":          tenant.Name,
			"app.kubernetes.io/managed-by": managedByValue,
			// The DMZ runs somebody else's traffic with no session in front of
			// it, so it gets the restricted posture without exception.
			"pod-security.kubernetes.io/enforce": "restricted",
		},
	}}
	err := r.Create(ctx, ns)
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// ensurePerimeterProxy is one published exposure: its configuration, the pod
// that serves it, a Service, the route on the perimeter Gateway, and the two
// network policies that bound what it may reach and who may reach the tenant.
func (r *ComponentReconciler) ensurePerimeterProxy(
	ctx context.Context, comp *gentianov1alpha1.Component, p *perimeterEnablement, dmz string,
) error {
	name := perimeterName(comp, p.spec.Name)
	labels := perimeterLabels(comp, p.spec.Name)

	upstream := fmt.Sprintf("%s.%s.svc.cluster.local", p.spec.Backend.Service, comp.Namespace)
	// Whether the caller's Authorization header goes on to the app is the
	// approval's word as well as the profile's: livePerimeterExposures let
	// this entry through only because the two agree.
	passCredential := p.spec.AuthMode == gentianov1alpha1.AuthModeApp &&
		p.on.Kind == gentianov1alpha1.ExposureKindPublicAppCredential
	config := perimeterProxyConfig(p.spec, upstream, p.spec.Backend.Port, p.website, passCredential, perimeterLimitsFromEnv(edgeClientAddressHeader(ctx, r.Client)))

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dmz, Labels: labels},
		Data:       map[string]string{"nginx.conf": config},
	}
	if err := upsert(ctx, r.Client, cm, func(existing *corev1.ConfigMap) bool {
		if equality.Semantic.DeepEqual(existing.Data, cm.Data) && equality.Semantic.DeepEqual(existing.Labels, cm.Labels) {
			return false
		}
		existing.Data, existing.Labels = cm.Data, cm.Labels
		return true
	}); err != nil {
		return err
	}

	// The configuration's hash on the pod template, so a change to the paths
	// a tenant publishes restarts the proxy. Without it nginx keeps serving
	// the old set and a revoked prefix stays open until something else
	// happens to restart the pod.
	sum := sha256.Sum256([]byte(config))
	deploy := perimeterDeployment(name, dmz, labels, hex.EncodeToString(sum[:])[:16])
	if err := upsert(ctx, r.Client, deploy, func(existing *appsv1.Deployment) bool {
		if equality.Semantic.DeepEqual(existing.Spec, deploy.Spec) {
			return false
		}
		existing.Spec = deploy.Spec
		return true
	}); err != nil {
		return err
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dmz, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name: "http", Port: perimeterProxyPort,
				TargetPort: intstr.FromInt32(perimeterProxyPort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	if err := upsert(ctx, r.Client, svc, func(existing *corev1.Service) bool {
		if equality.Semantic.DeepEqual(existing.Spec.Ports, svc.Spec.Ports) &&
			equality.Semantic.DeepEqual(existing.Spec.Selector, svc.Spec.Selector) {
			return false
		}
		existing.Spec.Ports, existing.Spec.Selector = svc.Spec.Ports, svc.Spec.Selector
		return true
	}); err != nil {
		return err
	}

	if err := r.ensurePerimeterPolicies(ctx, comp, name, dmz, labels, p); err != nil {
		return err
	}
	return r.ensurePerimeterRoute(ctx, name, dmz, labels, p)
}

// perimeterDeployment is the proxy pod: one container, read-only, no service
// account token, everything dropped.
func perimeterDeployment(name, dmz string, labels map[string]string, configHash string) *appsv1.Deployment {
	replicas := int32(1)
	no, yes := false, true
	user := int64(101) // nginx-alpine's own unprivileged user
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dmz, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{"gentianos.io/config-hash": configHash},
				},
				Spec: corev1.PodSpec{
					// Nothing here calls the API server, and a token mounted
					// in a pod on the public internet is a credential waiting
					// to be found.
					AutomountServiceAccountToken: &no,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &yes,
						RunAsUser:    &user,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  "proxy",
						Image: kernel.PerimeterProxyImage(),
						Ports: []corev1.ContainerPort{{
							Name: "http", ContainerPort: perimeterProxyPort, Protocol: corev1.ProtocolTCP,
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							ReadOnlyRootFilesystem:   &yes,
							RunAsNonRoot:             &yes,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/etc/nginx/nginx.conf", SubPath: "nginx.conf", ReadOnly: true},
							{Name: "tmp", MountPath: "/tmp"},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
								Port: intstr.FromInt32(perimeterProxyPort),
							}},
							InitialDelaySeconds: 2, PeriodSeconds: 10,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: name},
							},
						}},
						// Read-only root, and nginx still wants to write.
						{Name: "tmp", VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						}},
					},
				},
			},
		},
	}
}

// ensurePerimeterPolicies bound the DMZ in both directions, and who may
// reach the proxy.
//
// Out: the proxy may reach exactly one Service in the tenant's namespace, and
// DNS. Not the tenant's database, not its other applications, not the kernel.
// A proxy that is taken can make the requests its URLs already allow and
// nothing else.
//
// In: the tenant's namespace accepts traffic from the DMZ to that one
// component. The tenant baseline closes the namespace, so without this the
// proxy's requests are refused — which is the right default and the reason
// this policy is written rather than assumed.
func (r *ComponentReconciler) ensurePerimeterPolicies(
	ctx context.Context, comp *gentianov1alpha1.Component,
	name, dmz string, labels map[string]string, p *perimeterEnablement,
) error {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dns := intstr.FromInt32(53)
	port := intstr.FromInt32(p.spec.Backend.Port)
	proxyPort := intstr.FromInt32(perimeterProxyPort)

	egress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-egress", Namespace: dmz, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": comp.Namespace,
						}},
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							componentInstanceLabel: releaseName(comp),
						}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
				},
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns},
				}},
			},
		},
	}
	if err := upsert(ctx, r.Client, egress, func(existing *networkingv1.NetworkPolicy) bool {
		if equality.Semantic.DeepEqual(existing.Spec, egress.Spec) {
			return false
		}
		existing.Spec = egress.Spec
		return true
	}); err != nil {
		return err
	}

	// In, to the proxy itself: the Gateway's Envoy pods and nobody else.
	//
	// The proxy takes the caller's address from what the Gateway appended
	// to X-Forwarded-For, limits each address by it and tells the
	// application that address. That is only the Gateway's word if the
	// Gateway is the only thing that can open a connection here; a pod of
	// another tenant that could would choose its own address, and step
	// round the route's host and the limits with it. Under the cluster's
	// switch for the kernel's network rules (kernelNetworkPoliciesEnabled).
	edge := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-ingress", Namespace: dmz, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": layout.Namespace(layout.Edge),
					}},
					PodSelector: &metav1.LabelSelector{MatchLabels: netpolicy.EdgeProxyPodLabels()},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &proxyPort}},
			}},
		},
	}
	if !kernelNetworkPoliciesEnabled() {
		// The cluster's switch for the kernel's network rules is off, and
		// this rule rests on the same fact as they do: gone with them.
		if err := r.Delete(ctx, edge); client.IgnoreNotFound(err) != nil {
			return err
		}
	} else if err := upsert(ctx, r.Client, edge, func(existing *networkingv1.NetworkPolicy) bool {
		if equality.Semantic.DeepEqual(existing.Spec, edge.Spec) {
			return false
		}
		existing.Spec = edge.Spec
		return true
	}); err != nil {
		return err
	}

	ingress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-ingress", Namespace: comp.Namespace, Labels: labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				componentInstanceLabel: releaseName(comp),
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": dmz,
					}},
					PodSelector: &metav1.LabelSelector{MatchLabels: labels},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}
	return upsert(ctx, r.Client, ingress, func(existing *networkingv1.NetworkPolicy) bool {
		if equality.Semantic.DeepEqual(existing.Spec, ingress.Spec) {
			return false
		}
		existing.Spec = ingress.Spec
		return true
	})
}

// ensurePerimeterRoute publishes the host on the PERIMETER Gateway, which is
// the edge that carries no session. Putting it on the authenticated Gateway
// would sit a login in front of a link whose whole purpose is to work without
// one.
func (r *ComponentReconciler) ensurePerimeterRoute(
	ctx context.Context, name, dmz string, labels map[string]string, p *perimeterEnablement,
) error {
	edge := gatewayv1.Namespace(layout.Namespace(layout.Edge))
	gwName := gatewayv1.ObjectName(PerimeterGatewayName)
	svcKind := gatewayv1.Kind("Service")
	port := gatewayv1.PortNumber(perimeterProxyPort)
	section := gatewayv1.SectionName(perimeterListenerName(p.host))

	var rules []gatewayv1.HTTPRouteRule
	for _, prefix := range perimeterRoutePrefixes(p) {
		pathType := gatewayv1.PathMatchPathPrefix
		value := prefix
		rules = append(rules, gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &value},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(name), Kind: &svcKind, Port: &port,
				}},
			}},
		})
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dmz, Labels: labels},
		Spec: gatewayv1.HTTPRouteSpec{
			// The host's own listener, by name. Without it the route attaches
			// to every listener of the Gateway that admits its hostname, and
			// the :80 listener admits any: the surface would then be served
			// in clear text instead of being redirected to https.
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Name: gwName, Namespace: &edge, SectionName: &section,
			}}},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(p.host)},
			Rules:     rules,
		},
	}
	return upsert(ctx, r.Client, route, func(existing *gatewayv1.HTTPRoute) bool {
		if equality.Semantic.DeepEqual(existing.Spec, route.Spec) {
			return false
		}
		existing.Spec = route.Spec
		return true
	})
}

// perimeterRoutePrefixes are the prefixes one published surface is routed
// for: what its profile declared.
//
// One exception. The platform's own surface on the bare domain steps back
// while a tenant's website holds that address: it is then routed for the
// platform's paths only (mainAddressPlatformPrefixes), as far as it declared
// them, so that the website's whole-host route has no equal to tie with.
func perimeterRoutePrefixes(p *perimeterEnablement) []string {
	declared := perimeterPrefixes(p.spec)
	if !p.stepsBack {
		return declared
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(prefix string) {
		if _, dup := seen[prefix]; !dup {
			seen[prefix] = struct{}{}
			out = append(out, prefix)
		}
	}
	for _, kept := range mainAddressPlatformPrefixes() {
		for _, d := range declared {
			switch {
			case pathWithin(kept, d):
				add(kept)
			case pathWithin(d, kept):
				add(d)
			}
		}
	}
	sort.Strings(out)
	return out
}

// mainAddressFor decides what the cluster's main address means for this
// component, and writes the answer on its MainAddress condition (in memory:
// the caller's status update carries it).
//
// Asked only of a component whose profile has an apex entry, or one of whose
// surfaces was published for the main address. Every other component has
// nothing to do with it and reads nothing.
func (r *ComponentReconciler) mainAddressFor(
	ctx context.Context, comp *gentianov1alpha1.Component,
	profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant,
) (perimeterMainAddress, error) {
	concerned := false
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		concerned = concerned || (e.Surface == gentianov1alpha1.SurfacePerimeter && e.Apex)
	}
	for i := range comp.Spec.Exposures {
		concerned = concerned || comp.Spec.Exposures[i].Apex
	}
	if !concerned {
		apimeta.RemoveStatusCondition(&comp.Status.Conditions, ConditionMainAddress)
		return perimeterMainAddress{}, nil
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return perimeterMainAddress{}, err
	}
	profileList := &gentianov1alpha1.ComponentProfileList{}
	if err := r.List(ctx, profileList); err != nil {
		return perimeterMainAddress{}, err
	}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{}
	for i := range profileList.Items {
		profiles[profileList.Items[i].Name] = &profileList.Items[i]
	}
	profiles[comp.Name] = profile
	in := mainAddressInputs{
		Tenants: tenants.Items, Profiles: profiles,
		KernelDomain: r.KernelDomain, KernelRealm: r.kernelRealm(), TenancyMode: r.TenancyMode,
		Now: time.Now(),
	}
	main := perimeterMainAddress{held: mainAddressHolder(in) != nil}
	if tenantAdoptsKernelRealm(tenant, r.kernelRealm()) {
		// The platform's own page: it is on the bare domain either way, and
		// has no decision to report.
		apimeta.RemoveStatusCondition(&comp.Status.Conditions, ConditionMainAddress)
		return main, nil
	}
	verdict, applies := mainAddressVerdictFor(in, tenant, comp.Name)
	if !applies {
		apimeta.RemoveStatusCondition(&comp.Status.Conditions, ConditionMainAddress)
		return main, nil
	}
	status := metav1.ConditionFalse
	if verdict.Entry != "" {
		status = metav1.ConditionTrue
		main.entry, main.host = verdict.Entry, r.KernelDomain
	}
	apimeta.SetStatusCondition(&comp.Status.Conditions, metav1.Condition{
		Type: ConditionMainAddress, Status: status, Reason: verdict.Reason, Message: verdict.Message,
		ObservedGeneration: comp.Generation,
	})
	return main, nil
}

// prunePerimeter removes what the enablements no longer say.
//
// This is what makes an expiry, a revocation and a removed path actually stop
// answering. A published surface that outlived its enablement because nobody
// deleted the Deployment would be the worst failure this whole mechanism can
// have: a link somebody revoked that still works.
func (r *ComponentReconciler) prunePerimeter(
	ctx context.Context, comp *gentianov1alpha1.Component, dmz string, live []perimeterEnablement,
) error {
	keep := map[string]struct{}{}
	for i := range live {
		keep[perimeterName(comp, live[i].spec.Name)] = struct{}{}
	}
	selector := client.MatchingLabels{"gentianos.io/component": comp.Name, "app.kubernetes.io/name": perimeterProxyAppName}

	routes := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, routes, client.InNamespace(dmz), selector); err == nil {
		for i := range routes.Items {
			if _, ok := keep[routes.Items[i].Name]; !ok {
				if err := r.Delete(ctx, &routes.Items[i]); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
		}
	}
	deploys := &appsv1.DeploymentList{}
	if err := r.List(ctx, deploys, client.InNamespace(dmz), selector); err == nil {
		for i := range deploys.Items {
			if _, ok := keep[deploys.Items[i].Name]; !ok {
				if err := r.Delete(ctx, &deploys.Items[i]); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
		}
	}
	svcs := &corev1.ServiceList{}
	if err := r.List(ctx, svcs, client.InNamespace(dmz), selector); err == nil {
		for i := range svcs.Items {
			if _, ok := keep[svcs.Items[i].Name]; !ok {
				if err := r.Delete(ctx, &svcs.Items[i]); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
		}
	}
	cms := &corev1.ConfigMapList{}
	if err := r.List(ctx, cms, client.InNamespace(dmz), selector); err == nil {
		for i := range cms.Items {
			if _, ok := keep[cms.Items[i].Name]; !ok {
				if err := r.Delete(ctx, &cms.Items[i]); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
		}
	}
	// The policies, in both namespaces.
	for _, ns := range []string{dmz, comp.Namespace} {
		policies := &networkingv1.NetworkPolicyList{}
		if err := r.List(ctx, policies, client.InNamespace(ns), selector); err != nil {
			continue
		}
		for i := range policies.Items {
			base := policies.Items[i].Name
			for _, suffix := range []string{"-egress", "-ingress"} {
				if len(base) > len(suffix) && base[len(base)-len(suffix):] == suffix {
					base = base[:len(base)-len(suffix)]
					break
				}
			}
			if _, ok := keep[base]; !ok {
				if err := r.Delete(ctx, &policies.Items[i]); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
		}
	}
	return nil
}

// upsert creates the object, or applies mutate to what is already there and
// patches when mutate says something changed.
func upsert[T client.Object](ctx context.Context, c client.Client, desired T, mutate func(T) bool) error {
	existing := desired.DeepCopyObject().(T)
	err := c.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopyObject().(T))
	if !mutate(existing) {
		return nil
	}
	return c.Patch(ctx, existing, patch)
}
