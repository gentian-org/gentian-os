/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package kernelnet is who may open a connection to a pod of a kernel
// namespace: the list (inventory.yaml), and the NetworkPolicies made from it.
//
// The list comes first and the rules are derived, so that a rule cannot
// exist without a line that says who it is for and where that was read. The
// manifest the installer applies is generated from here
// (kernel/security/network-policies/kernel-network-policies.yaml) and a test
// keeps the two equal.
//
// The rules are networking.k8s.io/v1 NetworkPolicy and nothing else, so that
// they mean the same on every network plugin that enforces it. Per kernel
// namespace: one policy that selects every pod, which is what denies an
// ingress nothing lists, with one rule per group of ports that share their
// callers. Ports are the POD's ports: a plugin matches a connection after
// the Service's address and port have been replaced by a pod's.
package kernelnet

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/layout"
)

//go:embed inventory.yaml
var inventoryYAML []byte

const (
	// PolicyLabel marks every policy made here, and is how the installer
	// finds them: to remove one a later list no longer has, and to remove
	// all of them when the switch is off.
	PolicyLabel = "gentianos.io/kernel-network-policy"
	// NamespacePolicyName is the policy that selects every pod of a kernel
	// namespace.
	NamespacePolicyName = "kernel-ingress"

	nameLabel = "kubernetes.io/metadata.name"
)

// Inventory is inventory.yaml.
type Inventory struct {
	Excluded   []Excluded      `json:"excluded"`
	Unmanaged  []Unmanaged     `json:"unmanaged"`
	Peers      map[string]Peer `json:"peers"`
	Namespaces []Namespace     `json:"namespaces"`
}

// Excluded is a namespace whose rules somebody else writes.
type Excluded struct {
	Namespace string `json:"namespace"`
	Owner     string `json:"owner"`
	Why       string `json:"why"`
}

// Unmanaged is a kernel-tier namespace the platform does not own.
type Unmanaged struct {
	Namespace string `json:"namespace"`
	Why       string `json:"why"`
}

// Peer is a caller: the pods of one namespace (all, or those with the
// labels), of several namespaces by name, or of every namespace of a tier.
type Peer struct {
	What       string            `json:"what"`
	Namespace  string            `json:"namespace,omitempty"`
	Namespaces []string          `json:"namespaces,omitempty"`
	Tier       []string          `json:"tier,omitempty"`
	PodLabels  map[string]string `json:"podLabels,omitempty"`
	Evidence   []string          `json:"evidence,omitempty"`
}

// Namespace is one kernel namespace.
type Namespace struct {
	Name string `json:"name"`
	// SameNamespace admits every pod of the namespace to every other, on
	// every port.
	SameNamespace    bool       `json:"sameNamespace"`
	SameNamespaceWhy string     `json:"sameNamespaceWhy"`
	Egress           string     `json:"egress"`
	EgressWhy        string     `json:"egressWhy"`
	Workloads        []Workload `json:"workloads"`
}

// Workload is one set of pods.
type Workload struct {
	Name        string            `json:"name"`
	InstalledBy string            `json:"installedBy"`
	ReadAt      string            `json:"readAt"`
	PodLabels   map[string]string `json:"podLabels"`
	// AllIngress admits any source on any port, by a policy of the
	// workload's own that selects its pods by PodLabels.
	AllIngress string   `json:"allIngress,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
	Ports      []Port   `json:"ports"`
}

// Port is one port a workload's pods listen on, and what admits to it.
type Port struct {
	Port      int32    `json:"port"`
	What      string   `json:"what"`
	From      []From   `json:"from,omitempty"`
	Open      string   `json:"open,omitempty"`
	Closed    string   `json:"closed,omitempty"`
	Elsewhere string   `json:"elsewhere,omitempty"`
	Also      []string `json:"also,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
}

// From is one admitted caller of a port.
type From struct {
	Peer     string   `json:"peer"`
	Why      string   `json:"why"`
	Evidence []string `json:"evidence"`
}

// Load reads the embedded inventory and refuses one that does not say, for
// every port, exactly one thing.
func Load() (*Inventory, error) {
	inv := &Inventory{}
	if err := yaml.UnmarshalStrict(inventoryYAML, inv); err != nil {
		return nil, fmt.Errorf("inventory.yaml: %w", err)
	}
	if err := inv.validate(); err != nil {
		return nil, fmt.Errorf("inventory.yaml: %w", err)
	}
	return inv, nil
}

func (inv *Inventory) validate() error {
	seen := map[string]bool{}
	for _, ns := range inv.Namespaces {
		if seen[ns.Name] {
			return fmt.Errorf("%s is listed twice", ns.Name)
		}
		seen[ns.Name] = true
		if ns.SameNamespaceWhy == "" || ns.EgressWhy == "" {
			return fmt.Errorf("%s: sameNamespaceWhy and egressWhy are required", ns.Name)
		}
		if ns.Egress != "open" {
			return fmt.Errorf("%s: egress is %q; these rules restrict no egress", ns.Name, ns.Egress)
		}
		// A rule is by port for the whole namespace, so two workloads that
		// share a port number share its callers. One saying closed while
		// another admits somebody would be a line that is not true.
		admitted := map[int32]string{}
		closed := map[int32]string{}
		for _, w := range ns.Workloads {
			if w.InstalledBy == "" || w.ReadAt == "" || len(w.PodLabels) == 0 {
				return fmt.Errorf("%s/%s: installedBy, readAt and podLabels are required", ns.Name, w.Name)
			}
			for _, p := range w.Ports {
				at := fmt.Sprintf("%s/%s port %d", ns.Name, w.Name, p.Port)
				said := 0
				for _, set := range []bool{len(p.From) > 0, p.Open != "", p.Closed != "", p.Elsewhere != ""} {
					if set {
						said++
					}
				}
				if said != 1 {
					return fmt.Errorf("%s: exactly one of from, open, closed, elsewhere", at)
				}
				for _, f := range p.From {
					if _, ok := inv.Peers[f.Peer]; !ok {
						return fmt.Errorf("%s: no peer %q", at, f.Peer)
					}
					if f.Why == "" || len(f.Evidence) == 0 {
						return fmt.Errorf("%s: peer %s needs why and evidence", at, f.Peer)
					}
				}
				if w.AllIngress != "" {
					if p.Open == "" {
						return fmt.Errorf("%s: a workload with allIngress has only open ports", at)
					}
					continue
				}
				if len(p.From) > 0 || p.Open != "" {
					admitted[p.Port] = w.Name
				}
				if p.Closed != "" {
					closed[p.Port] = w.Name
				}
			}
		}
		for port, w := range closed {
			if other, ok := admitted[port]; ok {
				return fmt.Errorf("%s: port %d is closed for %s and admitted for %s, and the rule is by port for the namespace", ns.Name, port, w, other)
			}
		}
	}
	for _, name := range layout.KernelNamespaces() {
		if !seen[name] {
			return fmt.Errorf("kernel namespace %s has no entry", name)
		}
		delete(seen, name)
	}
	for name := range seen {
		return fmt.Errorf("%s is not a kernel namespace (internal/layout)", name)
	}
	for _, e := range inv.Excluded {
		for _, ns := range inv.Namespaces {
			if ns.Name == e.Namespace {
				return fmt.Errorf("%s is excluded and has rules", e.Namespace)
			}
		}
	}
	return nil
}

func (p Peer) networkPolicyPeers() []networkingv1.NetworkPolicyPeer {
	var pods *metav1.LabelSelector
	if len(p.PodLabels) > 0 {
		pods = &metav1.LabelSelector{MatchLabels: p.PodLabels}
	}
	var out []networkingv1.NetworkPolicyPeer
	names := append([]string{}, p.Namespaces...)
	if p.Namespace != "" {
		names = append(names, p.Namespace)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{nameLabel: name}},
			PodSelector:       pods,
		})
	}
	for _, tier := range p.Tier {
		out = append(out, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{layout.LabelTier: tier}},
			PodSelector:       pods,
		})
	}
	return out
}

func tcp(port int32) networkingv1.NetworkPolicyPort {
	proto := corev1.ProtocolTCP
	p := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}

func policyLabels(ns string) map[string]string {
	labels := map[string]string{
		PolicyLabel:                    "true",
		"app.kubernetes.io/managed-by": "gentian-os",
		layout.LabelTier:               string(layout.TierKernel),
	}
	if fn := strings.TrimPrefix(ns, "kernel-"); fn != ns {
		labels[layout.LabelFunction] = fn
	}
	return labels
}

// Policies are the NetworkPolicies of every kernel namespace, in the order
// of the inventory.
func (inv *Inventory) Policies() []*networkingv1.NetworkPolicy {
	var out []*networkingv1.NetworkPolicy
	for _, ns := range inv.Namespaces {
		// A workload that admits everything comes before the policy that
		// refuses by default, so that applying the file to a running
		// cluster never leaves its pods selected by the second alone.
		for _, w := range ns.Workloads {
			if w.AllIngress == "" {
				continue
			}
			out = append(out, &networkingv1.NetworkPolicy{
				TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
				ObjectMeta: metav1.ObjectMeta{
					Name: NamespacePolicyName + "-" + w.Name, Namespace: ns.Name, Labels: policyLabels(ns.Name),
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: w.PodLabels},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					Ingress:     []networkingv1.NetworkPolicyIngressRule{{}},
				},
			})
		}
		out = append(out, inv.namespacePolicy(ns))
	}
	return out
}

// namespacePolicy selects every pod of the namespace, which denies whatever
// no rule lists, and carries one rule per set of ports with the same callers.
func (inv *Inventory) namespacePolicy(ns Namespace) *networkingv1.NetworkPolicy {
	var rules []networkingv1.NetworkPolicyIngressRule
	if ns.SameNamespace {
		rules = append(rules, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
		})
	}

	// Per port: open, or the set of peers admitted.
	open := map[int32]bool{}
	peersOf := map[int32]map[string]bool{}
	for _, w := range ns.Workloads {
		if w.AllIngress != "" {
			continue
		}
		for _, p := range w.Ports {
			if p.Open != "" {
				open[p.Port] = true
			}
			for _, f := range p.From {
				if peersOf[p.Port] == nil {
					peersOf[p.Port] = map[string]bool{}
				}
				peersOf[p.Port][f.Peer] = true
			}
		}
	}

	var openPorts []int32
	for port := range open {
		openPorts = append(openPorts, port)
		delete(peersOf, port)
	}
	sort.Slice(openPorts, func(i, j int) bool { return openPorts[i] < openPorts[j] })
	if len(openPorts) > 0 {
		rule := networkingv1.NetworkPolicyIngressRule{}
		for _, port := range openPorts {
			rule.Ports = append(rule.Ports, tcp(port))
		}
		rules = append(rules, rule)
	}

	// Ports that share their callers share a rule.
	byPeers := map[string][]int32{}
	for port, peers := range peersOf {
		names := make([]string, 0, len(peers))
		for name := range peers {
			names = append(names, name)
		}
		sort.Strings(names)
		key := strings.Join(names, ",")
		byPeers[key] = append(byPeers[key], port)
	}
	keys := make([]string, 0, len(byPeers))
	for key, ports := range byPeers {
		sort.Slice(ports, func(a, b int) bool { return ports[a] < ports[b] })
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if pi, pj := byPeers[keys[i]][0], byPeers[keys[j]][0]; pi != pj {
			return pi < pj
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys {
		rule := networkingv1.NetworkPolicyIngressRule{}
		for _, name := range strings.Split(key, ",") {
			rule.From = append(rule.From, inv.Peers[name].networkPolicyPeers()...)
		}
		for _, port := range byPeers[key] {
			rule.Ports = append(rule.Ports, tcp(port))
		}
		rules = append(rules, rule)
	}

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name: NamespacePolicyName, Namespace: ns.Name, Labels: policyLabels(ns.Name),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     rules,
		},
	}
}

// Manifest is the policies as the one file the installer applies.
func (inv *Inventory) Manifest() ([]byte, error) {
	var b strings.Builder
	b.WriteString("# Generated by `make gen-kernel-network-policies` from\n")
	b.WriteString("# internal/kernel/kernelnet/inventory.yaml. Do not edit: change the\n")
	b.WriteString("# inventory, which says for every rule here who it is for and where\n")
	b.WriteString("# that was read, and generate again.\n")
	b.WriteString("#\n")
	b.WriteString("# Applied by the installer's first step with the namespaces themselves\n")
	b.WriteString("# (scripts/steps/A-01-namespaces.sh) when KERNEL_NETWORK_POLICIES=true;\n")
	b.WriteString("# otherwise that step removes every policy labelled\n")
	b.WriteString("# " + PolicyLabel + ".\n")
	for _, p := range inv.Policies() {
		raw, err := yaml.Marshal(p)
		if err != nil {
			return nil, err
		}
		b.WriteString("---\n")
		// The zero status and creation time say nothing and would only
		// show as a difference on every apply.
		for _, line := range strings.SplitAfter(string(raw), "\n") {
			if strings.HasPrefix(line, "status:") || strings.Contains(line, "creationTimestamp: null") {
				continue
			}
			b.WriteString(line)
		}
	}
	return []byte(b.String()), nil
}
