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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	runtimeMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// The TURN edge (networking.md §9): the relay that real-time media takes
// through NAT, for a cluster on which a component declares
// requires.services.turn.
//
// The Cluster composition composes system-turn-dmz once the engine is in
// the operator's set (system_engines.go). Everything in it is made here:
//
//   - the shared secret credentials are minted with, kept in the vault and
//     made once (secrets.Seeder), the same value every declaring component
//     is given (turn_credentials.go);
//   - the configuration of STUNner's daemon, run standalone: no STUNner
//     operator, no resource definitions of STUNner's, no permission outside
//     the namespace. The configuration is the one thing that names the
//     server software, which is why it is written here and nowhere else;
//   - the daemon's Deployment, a LoadBalancer Service of its own (not a
//     listener of the Gateway's, for the reason the mail edge has one), and
//     a headless Service its replicas find each other by;
//   - the certificate for turn.<kernel domain>, for TURN over TLS, and the
//     name's address record from the load balancer's address;
//   - the namespace's policy: the listeners from anywhere, the relay from
//     the edge's own pods, and out to nothing but those pods and DNS.
//
// The relay is headless: a client's relayed connection may reach the edge's
// own pods, which is how two participants behind NAT reach each other
// through one public port, and nothing else in the cluster.
//
// Nothing runs on a cluster whose edge is a tunnel: a tunnel carries no UDP
// and no TURN, and a component that declares the requirement there waits
// and says so (systemEngineReady).
// turnEdgeNamespace is the edge's namespace: a DMZ, named for its function.
var turnEdgeNamespace = layout.System("turn-dmz")

const (
	// turnEdgeName is the Deployment, the load balancer's Service and the
	// pods' label.
	turnEdgeName = "turn-edge"
	// turnEdgePeersService is headless: it resolves to the edge's pods, and
	// is the daemon's list of the peers a relay may reach.
	turnEdgePeersService = "turn-edge-peers"
	turnEdgeConfigSecret = "turn-edge-config"
	turnEdgeConfigKey    = "stunnerd.conf"
	turnEdgeTLSSecret    = "turn-edge-tls"

	// The ports clients reach. TURN over TLS is offered on 443, which
	// networks that let nothing else out still pass; the daemon binds an
	// unprivileged port for it.
	turnPort        int32 = 3478
	turnsPort       int32 = 443
	turnsListenPort int32 = 5349
	turnHealthPort  int32 = 8086

	// The vault record the shared secret is kept in:
	// gentian-os/kernel/turn/edge, property secret.
	turnSecretCategory = "turn"
	turnSecretName     = "edge"
	turnSecretProperty = "secret"
)

// turnUnreachableReason is what a component that declares TURN is told on a
// cluster whose edge is a tunnel.
const turnUnreachableReason = "the cluster's edge is a tunnel, which carries no UDP and no TURN. " +
	"Switch off what declares requires.services.turn, or run the cluster with networkMode static-ip"

// turnReachable reports whether anything can reach a TURN edge on this
// cluster: only one whose traffic arrives at its load balancers, with no
// ingress programmed in front of them. The same reading of the two settings
// as the operator's choice of ingress (cmd/main.go, buildEdgeIngress).
func turnReachable() bool {
	switch os.Getenv("EDGE_INGRESS") {
	case "none":
		return true
	case "":
		return os.Getenv("NETWORK_MODE") == "static-ip"
	}
	return false
}

// turnEdgeHost is the name clients reach the edge by.
func turnEdgeHost(kernelDomain string) string { return "turn." + kernelDomain }

// TURNEdgeReconciler keeps the TURN edge while the engine is wanted.
type TURNEdgeReconciler struct {
	client.Client
	// Namespace is the control namespace, where the engine set is.
	Namespace string
	// Seeder keeps the shared secret in the vault.
	Seeder *secrets.Seeder
	// KernelDomain is what the edge's name is under.
	KernelDomain string
	// ClusterIssuer issues the edge's certificate, by DNS-01: the name has
	// no HTTP behind it to answer a challenge on.
	ClusterIssuer string
}

var turnEdgeRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: turnEdgeName}}

func (r *TURNEdgeReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	set := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: systemEnginesConfigMap, Namespace: r.Namespace}, set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if set.Data[EngineTURN] != engineWanted || !turnReachable() {
		// Never wanted, purged, or nothing could reach it: what a purge
		// leaves goes with the namespace the composition stops composing.
		return ctrl.Result{}, nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: turnEdgeNamespace}, ns); err != nil {
		if errors.IsNotFound(err) {
			// Composed next; until then there is nowhere to write.
			return ctrl.Result{RequeueAfter: componentRequeue}, nil
		}
		return ctrl.Result{}, err
	}
	if r.KernelDomain == "" {
		return ctrl.Result{}, fmt.Errorf("no kernel domain for the TURN edge's name: KERNEL_DOMAIN is not set")
	}
	secret, err := turnSharedSecret(ctx, r.Seeder)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureCertificate(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("the TURN edge's certificate: %w", err)
	}
	cert, key, err := r.issuedCertificate(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	conf, err := turnEdgeConfig(turnEdgeHost(r.KernelDomain), secret, cert, key)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, obj := range turnEdgeObjects(kernel.TURNEdgeImage(), conf) {
		if err := r.apply(ctx, obj); err != nil {
			return ctrl.Result{}, fmt.Errorf("%T %s: %w", obj, obj.GetName(), err)
		}
	}
	if err := r.ensureDNS(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("the TURN edge's address record: %w", err)
	}
	// The certificate is issued and the address assigned in their own
	// time, and the edge is complete only with both.
	if len(cert) == 0 {
		return ctrl.Result{RequeueAfter: componentRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// turnSharedSecret is the secret every TURN credential is minted with, as the
// vault keeps it: made once, from the master or at random as the cluster's
// secret mode says, and read back ever after.
func turnSharedSecret(ctx context.Context, seeder *secrets.Seeder) (string, error) {
	if seeder == nil {
		return "", fmt.Errorf("the operator has no vault to keep the TURN edge's secret in")
	}
	return seeder.SeedKernelKey(ctx, turnSecretCategory, turnSecretName, turnSecretProperty)
}

// The daemon's configuration, as STUNner's v1 API spells it. Only what is
// set here is written; the daemon's own defaults are the rest.
type stunnerdConfig struct {
	Version   string             `json:"version"`
	Admin     stunnerdAdmin      `json:"admin"`
	Auth      stunnerdAuth       `json:"auth"`
	Listeners []stunnerdListener `json:"listeners"`
	Clusters  []stunnerdCluster  `json:"clusters"`
}

type stunnerdAdmin struct {
	Name     string `json:"name"`
	LogLevel string `json:"loglevel"`
}

type stunnerdAuth struct {
	Type        string            `json:"type"`
	Realm       string            `json:"realm"`
	Credentials map[string]string `json:"credentials"`
}

type stunnerdListener struct {
	Name     string   `json:"name"`
	Protocol string   `json:"protocol"`
	Address  string   `json:"address"`
	Port     int32    `json:"port"`
	Cert     string   `json:"cert,omitempty"`
	Key      string   `json:"key,omitempty"`
	Routes   []string `json:"routes"`
}

type stunnerdCluster struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Endpoints []string `json:"endpoints"`
}

// turnEdgeConfig is the daemon's configuration file.
//
// Every listener binds the pod's own address ($STUNNER_ADDR, which the daemon
// expands from its environment): a relayed connection is opened from the
// address it was asked on, and the edge's pods relay to one another by
// these. TURN over TLS is offered once the certificate is issued, and not
// before.
//
// The one cluster of peers is the headless Service of the edge's own pods,
// resolved by the daemon as it runs: a relay may reach those and nothing
// else. The namespace's egress policy says the same thing a second time.
func turnEdgeConfig(host, secret string, cert, key []byte) ([]byte, error) {
	// The daemon expands $NAME in the whole file. The secret the vault makes
	// is hex, and base64 has no "$" either; a secret somebody wrote into the
	// vault by hand might, and would be read as something else.
	if strings.Contains(secret, "$") {
		return nil, fmt.Errorf("the TURN edge's secret in the vault (%s) contains a \"$\", which the daemon would expand", secrets.KernelPath(turnSecretCategory, turnSecretName))
	}
	routes := []string{"peers"}
	listeners := []stunnerdListener{
		{Name: "turn-udp", Protocol: "TURN-UDP", Address: "$STUNNER_ADDR", Port: turnPort, Routes: routes},
		{Name: "turn-tcp", Protocol: "TURN-TCP", Address: "$STUNNER_ADDR", Port: turnPort, Routes: routes},
	}
	if len(cert) > 0 && len(key) > 0 {
		listeners = append(listeners, stunnerdListener{
			Name: "turn-tls", Protocol: "TURN-TLS", Address: "$STUNNER_ADDR", Port: turnsListenPort,
			Cert:   base64.StdEncoding.EncodeToString(cert),
			Key:    base64.StdEncoding.EncodeToString(key),
			Routes: routes,
		})
	}
	conf := stunnerdConfig{
		Version: "v1",
		Admin:   stunnerdAdmin{Name: turnEdgeName, LogLevel: "all:WARN"},
		Auth: stunnerdAuth{
			Type:        "ephemeral",
			Realm:       host,
			Credentials: map[string]string{"secret": secret},
		},
		Listeners: listeners,
		Clusters: []stunnerdCluster{{
			Name: "peers", Type: "STRICT_DNS",
			Endpoints: []string{fmt.Sprintf("%s.%s.svc.cluster.local", turnEdgePeersService, turnEdgeNamespace)},
		}},
	}
	return json.Marshal(conf)
}

func turnEdgeLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": turnEdgeName}
}

// turnEdgeObjects are the edge's workload, its two Services and the
// namespace's policy, for this configuration.
func turnEdgeObjects(image string, conf []byte) []client.Object {
	ns := turnEdgeNamespace
	labels := turnEdgeLabels()
	objLabels := map[string]string{managedByLabel: managedByValue, "app.kubernetes.io/name": turnEdgeName}
	om := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: objLabels}
	}
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	turnTarget, turnsTarget := intstr.FromInt32(turnPort), intstr.FromInt32(turnsListenPort)
	replicas := int32(2)
	no, yes := false, true
	uid := int64(65532)

	config := &corev1.Secret{
		ObjectMeta: om(turnEdgeConfigSecret),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{turnEdgeConfigKey: conf},
	}
	sa := &corev1.ServiceAccount{ObjectMeta: om(turnEdgeName), AutomountServiceAccountToken: &no}
	deployment := &appsv1.Deployment{
		ObjectMeta: om(turnEdgeName),
		Spec: appsv1.DeploymentSpec{
			// Two, on different nodes where there are two: the load balancer
			// sends only to a node that runs one (externalTrafficPolicy:
			// Local), and one replica is one node away from no calls.
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           turnEdgeName,
					AutomountServiceAccountToken: &no,
					Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
							Weight: 100,
							PodAffinityTerm: corev1.PodAffinityTerm{
								TopologyKey:   "kubernetes.io/hostname",
								LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
							},
						}},
					}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "stunnerd",
						Image: image,
						// -w: the daemon follows a changed file, so a renewed
						// certificate is taken without dropping the calls in
						// progress.
						Command: []string{"stunnerd", "-w", "-c", "/etc/stunnerd/" + turnEdgeConfigKey},
						Env: []corev1.EnvVar{{Name: "STUNNER_ADDR", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
						}}},
						Ports: []corev1.ContainerPort{
							{Name: "turn-udp", ContainerPort: turnPort, Protocol: udp},
							{Name: "turn-tcp", ContainerPort: turnPort, Protocol: tcp},
							{Name: "turn-tls", ContainerPort: turnsListenPort, Protocol: tcp},
							{Name: "health", ContainerPort: turnHealthPort, Protocol: tcp},
						},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/ready", Port: intstr.FromString("health")}}, PeriodSeconds: 5},
						LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/live", Port: intstr.FromString("health")}}, PeriodSeconds: 20},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/stunnerd", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: turnEdgeConfigSecret,
					}}}},
				},
			},
		},
	}
	// The one address of the edge, and the one Service of it with a load
	// balancer. externalTrafficPolicy: Local keeps the client's address,
	// which STUN reports back to the client; the cost is that only a node
	// running a replica serves, so the load balancer has to health-check
	// healthCheckNodePort.
	lb := &corev1.Service{
		ObjectMeta: om(turnEdgeName),
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			Selector:              labels,
			Ports: []corev1.ServicePort{
				{Name: "turn-udp", Port: turnPort, TargetPort: turnTarget, Protocol: udp},
				{Name: "turn-tcp", Port: turnPort, TargetPort: turnTarget, Protocol: tcp},
				{Name: "turn-tls", Port: turnsPort, TargetPort: turnsTarget, Protocol: tcp},
			},
		},
	}
	lb.Annotations = map[string]string{
		// As for the mail edge: without a health monitor Octavia sends to
		// every node, and most connections land where no replica runs.
		"loadbalancer.openstack.org/enable-health-monitor": "true",
	}
	peers := &corev1.Service{
		ObjectMeta: om(turnEdgePeersService),
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  labels,
			Ports:     []corev1.ServicePort{{Name: "turn-udp", Port: turnPort, TargetPort: turnTarget, Protocol: udp}},
		},
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: om(turnEdgeName),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				// The listeners, from anywhere: a client arrives through the
				// load balancer from the internet. The health port is not
				// listed; the kubelet asks from the pod's own node, which no
				// policy applies to.
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &turnTarget},
					{Protocol: &tcp, Port: &turnTarget},
					{Protocol: &tcp, Port: &turnsTarget},
				}},
				// A relayed connection, from another replica, on whichever
				// port the relay was given.
				{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: labels}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp}},
				},
			},
			// Out to the edge's own pods, which is all a relay may reach, and
			// to DNS, which resolves them. Nothing else of the cluster, and
			// nothing of the internet: a reply to a client goes back on the
			// connection it came in on.
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: labels}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp}},
				},
				dnsEgressRule(),
			},
		},
	}
	return []client.Object{config, sa, deployment, lb, peers, policy}
}

// dnsEgressRule is the way to the cluster's resolver: port 53 over UDP and
// TCP, in kube-system.
func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	port := intstr.FromInt32(53)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		}}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &port}, {Protocol: &tcp, Port: &port}},
	}
}

// ensureCertificate asks cert-manager for the edge's certificate, by DNS-01.
func (r *TURNEdgeReconciler) ensureCertificate(ctx context.Context) error {
	issuer := r.ClusterIssuer
	if issuer == "" {
		issuer = defaultDNS01ClusterIssuer
	}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(certManagerCertGVK)
	desired.SetName(turnEdgeTLSSecret)
	desired.SetNamespace(turnEdgeNamespace)
	desired.SetLabels(map[string]string{managedByLabel: managedByValue})
	spec := map[string]interface{}{
		"secretName": turnEdgeTLSSecret,
		"dnsNames":   []interface{}{turnEdgeHost(r.KernelDomain)},
		"issuerRef":  map[string]interface{}{"name": issuer, "kind": "ClusterIssuer"},
	}
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certManagerCertGVK)
	err := r.Get(ctx, types.NamespacedName{Name: turnEdgeTLSSecret, Namespace: turnEdgeNamespace}, existing)
	if runtimeMeta.IsNoMatchError(err) {
		// No cert-manager: TURN over TLS is not offered, and the rest is.
		return nil
	}
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Object["spec"] = desired.Object["spec"]
	return r.Patch(ctx, existing, patch)
}

// issuedCertificate is the edge's certificate and key, as cert-manager wrote
// them, or nothing while it has not.
func (r *TURNEdgeReconciler) issuedCertificate(ctx context.Context) ([]byte, []byte, error) {
	tls := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: turnEdgeTLSSecret, Namespace: turnEdgeNamespace}, tls)
	if errors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return tls.Data[corev1.TLSCertKey], tls.Data[corev1.TLSPrivateKeyKey], nil
}

// ensureDNS publishes turn.<kernel domain> at the load balancer's address,
// for external-dns: the name has no HTTP object behind it to be read from.
// Nothing is published while the address is not assigned.
func (r *TURNEdgeReconciler) ensureDNS(ctx context.Context) error {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: turnEdgeName, Namespace: turnEdgeNamespace}, svc); err != nil {
		return client.IgnoreNotFound(err)
	}
	var record map[string]interface{}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		switch {
		case in.IP != "":
			record = dnsEndpointRecord(turnEdgeHost(r.KernelDomain), "A", in.IP)
		case in.Hostname != "":
			record = dnsEndpointRecord(turnEdgeHost(r.KernelDomain), "CNAME", in.Hostname)
		}
		if record != nil {
			break
		}
	}
	if record == nil {
		return nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dnsEndpointGVK)
	obj.SetName(turnEdgeName)
	obj.SetNamespace(turnEdgeNamespace)
	obj.SetLabels(map[string]string{managedByLabel: managedByValue})
	if err := unstructured.SetNestedSlice(obj.Object, []interface{}{record}, "spec", "endpoints"); err != nil {
		return err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dnsEndpointGVK)
	err := r.Get(ctx, types.NamespacedName{Name: turnEdgeName, Namespace: turnEdgeNamespace}, existing)
	if runtimeMeta.IsNoMatchError(err) {
		return nil
	}
	if errors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Object["spec"], obj.Object["spec"]) {
		return nil
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, obj)
}

// apply writes obj as desired, over what an earlier reconcile left.
func (r *TURNEdgeReconciler) apply(ctx context.Context, desired client.Object) error {
	existing, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("not an object")
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.SetLabels(desired.GetLabels())
		switch e := existing.(type) {
		case *corev1.Secret:
			e.Type, e.Data = desired.(*corev1.Secret).Type, desired.(*corev1.Secret).Data
		case *corev1.ServiceAccount:
			e.AutomountServiceAccountToken = desired.(*corev1.ServiceAccount).AutomountServiceAccountToken
		case *appsv1.Deployment:
			e.Spec = desired.(*appsv1.Deployment).Spec
		case *corev1.Service:
			d := desired.(*corev1.Service)
			e.Annotations = d.Annotations
			e.Spec.Type, e.Spec.ExternalTrafficPolicy = d.Spec.Type, d.Spec.ExternalTrafficPolicy
			e.Spec.Selector, e.Spec.Ports = d.Spec.Selector, d.Spec.Ports
			if d.Spec.ClusterIP == corev1.ClusterIPNone {
				e.Spec.ClusterIP = corev1.ClusterIPNone
			}
		case *networkingv1.NetworkPolicy:
			e.Spec = desired.(*networkingv1.NetworkPolicy).Spec
		}
		return nil
	})
	return err
}

// turnEdgeReady reports whether the edge answers: its replicas are ready and
// its load balancer has an address clients can be sent to.
func turnEdgeReady(ctx context.Context, c client.Reader) (bool, error) {
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: turnEdgeName, Namespace: turnEdgeNamespace}, dep); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !deploymentIsReady(dep) {
		return false, nil
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: turnEdgeName, Namespace: turnEdgeNamespace}, svc); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if in.IP != "" || in.Hostname != "" {
			return true, nil
		}
	}
	return false, nil
}

func (r *TURNEdgeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toEdge := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{turnEdgeRequest}
	})
	inNamespace := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == turnEdgeNamespace })
	return ctrl.NewControllerManagedBy(mgr).
		Named("turn-edge").
		Watches(&corev1.ConfigMap{}, toEdge, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetName() == systemEnginesConfigMap && o.GetNamespace() == r.Namespace
		}))).
		Watches(&corev1.Namespace{}, toEdge, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetName() == turnEdgeNamespace
		}))).
		// The issued certificate, and the load balancer's address.
		Watches(&corev1.Secret{}, toEdge, builder.WithPredicates(inNamespace)).
		Watches(&corev1.Service{}, toEdge, builder.WithPredicates(inNamespace)).
		Watches(&appsv1.Deployment{}, toEdge, builder.WithPredicates(inNamespace)).
		Complete(r)
}

// The profile that declares TURN, by what it says.
func wantsTURN(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.TURN != nil
}
