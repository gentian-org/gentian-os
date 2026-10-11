/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	utiljson "k8s.io/apimachinery/pkg/util/json"
	yamlv3 "sigs.k8s.io/yaml/goyaml.v3"
)

// What a bundle is, and what it may hold.
//
// One profile, one file, one fingerprint. The file a catalogue publishes for
// an entry is a YAML stream: the ComponentProfile first, and after it the
// other objects the app needs on a cluster -- its COMPANIONS. The digest is
// taken over the whole file, so an install pinned to a digest is pinned to
// the profile and to every companion with it. A file holding the profile
// alone is a bundle with no companions, which is what every bundle was before
// there were any.
//
// Everything in the file is applied to the cluster with the rights of the
// catalogue's Argo CD Application, which are wide. So what may be in it is a
// short list, each kind with the one name it may have, and the list is held
// twice: by the director before it writes anything, and by the operator
// before it rolls anything out. Anything not on the list is refused -- a
// kind, a field, a label, a namespace. There is no "probably harmless".
//
// Every companion is named after its profile, in a shape only that profile's
// name produces. Two bundles therefore cannot claim one object, and a bundle
// cannot name an object that is the platform's or another profile's.
//
// And companions are a cluster catalogue's to publish. Each of them is an
// object of the whole cluster or lives in a namespace of the platform, where
// a tenant decides nothing: a bundle from a tenant's own catalogue is its
// profile and nothing else.

// ErrRefused is a bundle that holds what a bundle may not. It is not a
// mismatch -- the bytes may be exactly the build that was asked for -- and
// retrying changes nothing: the catalogue has to publish something else.
var ErrRefused = errors.New("the bundle holds what a bundle may not")

// ProfileLabel is the label every companion carries: the name of the profile
// it travels with. It is also how a Composition finds a profile and what
// came with it, which is why nothing in a bundle may carry another's.
const ProfileLabel = "gentianos.io/profile-name"

// AssetLabel names what a ConfigMap companion holds, for the Composition
// that looks for it.
const AssetLabel = "gentianos.io/asset"

// The companion kinds.
const (
	KindComposition   = "Composition"
	KindConfigMap     = "ConfigMap"
	KindCustomization = "Customization"
	KindOIDCPacks     = "OIDCPackCatalog"
)

const (
	gentianAPIVersion = "gentianos.io/v1alpha1"
	// compositionAPIVersion is the one version of Crossplane's Composition
	// this platform writes its own in.
	compositionAPIVersion = "apiextensions.crossplane.io/v1"
	// appComposite is the kind the platform composes an app from. A
	// Composition of any other kind creates something that is not an app.
	appComposite = "XApp"
	// defaultComposition renders every app that names no other. Nothing in
	// a bundle is ever it.
	defaultComposition = "app-default"
	// compositionPrefix is what the name of every app Composition begins
	// with, the platform's own included.
	compositionPrefix = "app-"
	// oidcSuffix ends the name of a profile's OIDC packs.
	oidcSuffix = "-oidc"
)

// dnsLabel is a profile's name, and the part of a companion's name that
// follows it. No dots: a dot is what separates the two.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Companion is one object a bundle holds beside its profile.
type Companion struct {
	APIVersion string
	Kind       string
	Name       string
	// Namespaced says the kind lives in a namespace. A companion states
	// none: it is applied where the catalogue is, and looked for there.
	Namespaced bool
	// Object is the document as the cluster received it.
	Object map[string]any
}

// String names a companion for a person.
func (c Companion) String() string { return c.Kind + " " + c.Name }

// Bundle is a bundle read into its documents.
type Bundle struct {
	// Name is the profile's metadata.name.
	Name string
	// Profile is the first document.
	Profile map[string]any
	// Companions are the others, in the order the file has them.
	Companions []Companion
}

// Composition is the name of the Composition the bundle carries, or "".
func (b *Bundle) Composition() string {
	for _, c := range b.Companions {
		if c.Kind == KindComposition {
			return c.Name
		}
	}
	return ""
}

// Check reads a bundle and holds it to what a bundle may be: one
// ComponentProfile called name, first, and after it only companions this
// profile may bring from the catalogue origin says it came from.
//
// origin is a catalogue's origin (ClusterOrigin or TenantOrigin). One that
// cannot be read, or none, is not a cluster catalogue, and brings no
// companions.
//
// Every refusal is ErrRefused with what was found.
func Check(body []byte, name, origin string) (*Bundle, error) {
	docs, err := documents(body)
	if err != nil {
		return nil, fmt.Errorf("%w: it does not parse: %v", ErrRefused, err)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("%w: it holds no document", ErrRefused)
	}
	b := &Bundle{Profile: docs[0]}
	head := headOf(docs[0])
	if head.kind != "ComponentProfile" {
		return nil, fmt.Errorf("%w: its first document is a %s, not a ComponentProfile", ErrRefused, orNothing(head.kind))
	}
	b.Name = head.name
	if head.name != name {
		return nil, fmt.Errorf("%w: its profile is named %q in the bundle, not %q", ErrRefused, head.name, name)
	}
	if err := checkProfile(b); err != nil {
		return nil, err
	}
	if len(docs) == 1 {
		return b, nil
	}

	// Companions, and so the questions a profile alone never raises.
	if !dnsLabel.MatchString(name) {
		return nil, fmt.Errorf("%w: a profile that brings companions has a short lower-case name, and %q is not one", ErrRefused, name)
	}
	from, err := ParseOrigin(origin)
	cluster := err == nil && from.Source != "" && from.Tenant == ""
	seen := map[string]bool{}
	for _, doc := range docs[1:] {
		head := headOf(doc)
		c := Companion{APIVersion: head.apiVersion, Kind: head.kind, Name: head.name, Object: doc}
		if head.kind == "ComponentProfile" {
			return nil, fmt.Errorf("%w: it holds a second ComponentProfile, %q; a bundle is one profile and what travels with it",
				ErrRefused, head.name)
		}
		rule, ok := rules[head.apiVersion+"/"+head.kind]
		if !ok {
			return nil, fmt.Errorf("%w: it holds a %s (%s), which is not a kind a bundle may hold; those are %s",
				ErrRefused, orNothing(head.kind), orNothing(head.apiVersion), allowedKinds())
		}
		if !cluster {
			return nil, fmt.Errorf("%w: it holds the %s, and only a catalogue of the whole cluster, which the cluster's "+
				"administrator adds, brings anything beside a profile; a tenant's own catalogue publishes profiles alone",
				ErrRefused, c)
		}
		c.Namespaced = rule.namespaced
		if err := rule.check(b, &c, rule); err != nil {
			return nil, err
		}
		if seen[c.Kind+"/"+c.Name] {
			return nil, fmt.Errorf("%w: it holds the %s twice", ErrRefused, c)
		}
		seen[c.Kind+"/"+c.Name] = true
		b.Companions = append(b.Companions, c)
	}
	return b, nil
}

// checkProfile holds the profile document to what no catalogue states.
//
// The label by which a Composition finds a profile is that profile's own
// name or absent: a profile carrying another's would be found in its place.
// And nothing addressed to Argo CD, which applies this document and would
// take a sync option or a hook from it.
func checkProfile(b *Bundle) error {
	meta, _ := b.Profile["metadata"].(map[string]any)
	for _, field := range []string{"labels", "annotations"} {
		entries, _ := meta[field].(map[string]any)
		for key, value := range entries {
			if strings.HasPrefix(key, argoPrefix) {
				return fmt.Errorf("%w: its profile states %s, which is addressed to Argo CD and not a catalogue's to set", ErrRefused, key)
			}
			if field == "labels" && key == ProfileLabel && value != b.Name {
				return fmt.Errorf("%w: its profile carries the label %s: %v, which is another profile's name", ErrRefused, key, value)
			}
		}
	}
	return nil
}

// argoPrefix is the namespace of what Argo CD reads from an object it applies.
const argoPrefix = "argocd.argoproj.io/"

// rule is what one kind of companion has to be.
type rule struct {
	kind       string
	namespaced bool
	// body is the one field beside apiVersion, kind and metadata a
	// companion of this kind states.
	body string
	// check holds the companion to its kind's own rule and answers the
	// labels it must carry beside ProfileLabel.
	check func(b *Bundle, c *Companion, r rule) error
}

// rules is the list: every kind a bundle may hold beside its profile, by
// apiVersion and kind. docs/custom-catalogues.md states it for a person; a
// kind is added in both or in neither.
var rules = map[string]rule{
	compositionAPIVersion + "/" + KindComposition: {kind: KindComposition, body: "spec", check: checkComposition},
	gentianAPIVersion + "/" + KindOIDCPacks:       {kind: KindOIDCPacks, body: "spec", check: checkOIDCPacks},
	"v1/" + KindConfigMap:                         {kind: KindConfigMap, namespaced: true, body: "data", check: checkConfigMap},
	gentianAPIVersion + "/" + KindCustomization:   {kind: KindCustomization, namespaced: true, body: "spec", check: checkCustomization},
}

func allowedKinds() string {
	kinds := make([]string, 0, len(rules))
	for _, r := range rules {
		kinds = append(kinds, r.kind)
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ", ")
}

// shape holds a companion to the form all of them have: its kind's one body
// field, a name, exactly the labels given -- and nothing else. No namespace,
// no annotations, no owner, no finalizer, no status.
func shape(c *Companion, r rule, labels map[string]string) error {
	for key := range c.Object {
		if key != "apiVersion" && key != "kind" && key != "metadata" && key != r.body {
			return fmt.Errorf("%w: the %s states %s; a %s companion states %s and nothing else", ErrRefused, c, key, r.kind, r.body)
		}
	}
	meta, _ := c.Object["metadata"].(map[string]any)
	for key := range meta {
		if key != "name" && key != "labels" {
			return fmt.Errorf("%w: the %s states metadata.%s; a companion states its name and its labels and nothing else, "+
				"and it is applied where the catalogue is, not where it says", ErrRefused, c, key)
		}
	}
	stated, _ := meta["labels"].(map[string]any)
	if len(stated) != len(labels) {
		return wrongLabels(c, labels)
	}
	for key, want := range labels {
		if got, ok := stated[key].(string); !ok || got != want {
			return wrongLabels(c, labels)
		}
	}
	return nil
}

func wrongLabels(c *Companion, labels map[string]string) error {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	said := make([]string, 0, len(keys))
	for _, key := range keys {
		said = append(said, key+": "+labels[key])
	}
	return fmt.Errorf("%w: the %s must carry exactly these labels: %s", ErrRefused, c, strings.Join(said, ", "))
}

// checkComposition: the one Composition this profile is rendered by.
//
// A Composition decides which objects exist for an app, and they are made
// with the providers' rights, which are the cluster's. So it is the app's
// own -- app-<profile>, named by the profile's spec.package.composition --
// it composes an app and nothing else, and it is never app-default.
func checkComposition(b *Bundle, c *Companion, r rule) error {
	if c.Name == defaultComposition {
		return fmt.Errorf("%w: it holds the Composition %s, which is the platform's and is replaced by nothing", ErrRefused, c.Name)
	}
	if want := compositionPrefix + b.Name; c.Name != want {
		return fmt.Errorf("%w: its Composition is named %q; the Composition of profile %s is %s", ErrRefused, c.Name, b.Name, want)
	}
	spec, _ := c.Object["spec"].(map[string]any)
	ref, _ := spec["compositeTypeRef"].(map[string]any)
	if ref["apiVersion"] != gentianAPIVersion || ref["kind"] != appComposite {
		return fmt.Errorf("%w: the %s composes %v (%v); a bundle's Composition composes %s (%s) and nothing else",
			ErrRefused, c, orNothing(ref["kind"]), orNothing(ref["apiVersion"]), appComposite, gentianAPIVersion)
	}
	profileSpec, _ := b.Profile["spec"].(map[string]any)
	pkg, _ := profileSpec["package"].(map[string]any)
	if pkg["composition"] != c.Name {
		return fmt.Errorf("%w: the %s is not the one its profile is rendered by (spec.package.composition: %v)",
			ErrRefused, c, orNothing(pkg["composition"]))
	}
	return shape(c, r, map[string]string{ProfileLabel: b.Name})
}

// checkOIDCPacks: the packs for the clients this profile declares.
//
// A pack is found by its client's id, across the cluster, and says what the
// identity provider puts in that client's tokens in every tenant's realm. A
// profile brings packs for its own clients only, and none for a service
// client, which is how the platform's own backends authenticate.
func checkOIDCPacks(b *Bundle, c *Companion, r rule) error {
	if want := b.Name + oidcSuffix; c.Name != want {
		return fmt.Errorf("%w: its OIDCPackCatalog is named %q; that of profile %s is %s", ErrRefused, c.Name, b.Name, want)
	}
	clients := map[string]bool{}
	stringsAt(b.Profile["spec"], clients, "clientId", "oidcPackRef")
	spec, _ := c.Object["spec"].(map[string]any)
	packs, _ := spec["packs"].(map[string]any)
	for key, value := range packs {
		if !clients[key] {
			return fmt.Errorf("%w: the %s holds a pack for the client %q, which this profile does not declare", ErrRefused, c, key)
		}
		if pack, _ := value.(map[string]any); pack["serviceClient"] == true {
			return fmt.Errorf("%w: the %s declares %q a service client, which is the platform's to declare", ErrRefused, c, key)
		}
	}
	return shape(c, r, map[string]string{ProfileLabel: b.Name})
}

// checkConfigMap: a file the profile's Composition reads.
//
// Named <profile>.<asset> and found by its two labels. It carries no other
// label: the platform's own configuration is found by label too, and a
// ConfigMap that could say it was that would be read as that.
func checkConfigMap(b *Bundle, c *Companion, r rule) error {
	meta, _ := c.Object["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	asset, _ := labels[AssetLabel].(string)
	if !dnsLabel.MatchString(asset) {
		return fmt.Errorf("%w: the %s needs the label %s, a short lower-case name for what it holds", ErrRefused, c, AssetLabel)
	}
	if want := b.Name + "." + asset; c.Name != want {
		return fmt.Errorf("%w: its ConfigMap for %q is named %q; that of profile %s is %s", ErrRefused, asset, c.Name, b.Name, want)
	}
	if c.Name == rootCABundle {
		return fmt.Errorf("%w: the ConfigMap %s is the cluster's own in every namespace", ErrRefused, c.Name)
	}
	data, _ := c.Object["data"].(map[string]any)
	for key, value := range data {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%w: the %s has something other than text under data.%s", ErrRefused, c, key)
		}
	}
	return shape(c, r, map[string]string{ProfileLabel: b.Name, AssetLabel: asset})
}

// rootCABundle is the one ConfigMap Kubernetes keeps in every namespace, and
// the one name of the platform's that has the shape of a companion's.
const rootCABundle = "kube-root-ca.crt"

// checkCustomization: a record of a deviation this profile carries.
//
// It is a record and changes nothing; the operator reads it for what the
// cluster owes in maintenance. It is about this profile and at the profile's
// scope, since a record about another app or about the platform is not this
// catalogue entry's to file.
func checkCustomization(b *Bundle, c *Companion, r rule) error {
	prefix, record, _ := strings.Cut(c.Name, ".")
	if prefix != b.Name || !dnsLabel.MatchString(record) {
		return fmt.Errorf("%w: its Customization is named %q; those of profile %s are %s.<record>", ErrRefused, c.Name, b.Name, b.Name)
	}
	spec, _ := c.Object["spec"].(map[string]any)
	target, _ := spec["target"].(map[string]any)
	if target["profile"] != b.Name {
		return fmt.Errorf("%w: the %s is about %v, not about the profile it travels with", ErrRefused, c, orNothing(target["profile"]))
	}
	if spec["scope"] != "profile" {
		return fmt.Errorf("%w: the %s has scope %v; a bundle carries records of scope profile", ErrRefused, c, orNothing(spec["scope"]))
	}
	return shape(c, r, map[string]string{ProfileLabel: b.Name})
}

// stringsAt collects every string found under one of keys, anywhere in v.
func stringsAt(v any, into map[string]bool, keys ...string) {
	switch x := v.(type) {
	case map[string]any:
		for key, value := range x {
			if s, ok := value.(string); ok && s != "" {
				for _, k := range keys {
					if key == k {
						into[s] = true
					}
				}
			}
			stringsAt(value, into, keys...)
		}
	case []any:
		for _, value := range x {
			stringsAt(value, into, keys...)
		}
	}
}

type head struct{ apiVersion, kind, name string }

func headOf(doc map[string]any) head {
	meta, _ := doc["metadata"].(map[string]any)
	h := head{}
	h.apiVersion, _ = doc["apiVersion"].(string)
	h.kind, _ = doc["kind"].(string)
	h.name, _ = meta["name"].(string)
	return h
}

func orNothing(v any) any {
	if v == nil || v == "" {
		return "nothing"
	}
	return v
}

// documents reads the YAML documents of a bundle that hold anything. A
// separator with nothing after it, or a document of comments only, is not
// one.
//
// Each is read the way kustomize reads it, because the catalogue directory
// is a kustomization and that is the reading the cluster received: a bare
// `yes` or `on` is a string there, where the YAML 1.1 reading kubectl
// applies would make it a boolean, and a bare date is a timestamp. Whole
// numbers stay whole: a port read as 8080.0 would not fit its field.
func documents(body []byte) ([]map[string]any, error) {
	dec := yamlv3.NewDecoder(bytes.NewReader(body))
	var out []map[string]any
	for {
		var read any
		err := dec.Decode(&read)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if read == nil {
			continue
		}
		raw, err := json.Marshal(read)
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if err := utiljson.Unmarshal(raw, &doc); err != nil {
			return nil, errors.New("a document is not an object")
		}
		if doc == nil {
			return nil, errors.New("a document is not an object")
		}
		out = append(out, doc)
	}
}
