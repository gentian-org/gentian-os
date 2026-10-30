/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// renderValues renders the app's release and what its values read: the
// sensitive Secret, the supplied credentials, the keep rule and the Service
// aliases.
func renderValues(in *Input, out *Rendered) error {
	r := &valuesRender{
		in:     in,
		spec:   in.Profile,
		tenant: in.Tenant(),
		ns:     in.Namespace,
	}
	if r.spec.Package.ValueMapping != nil {
		r.vm = r.spec.Package.ValueMapping
	} else {
		r.vm = &gentianov1alpha1.ValueMapping{}
	}
	if r.spec.Package.Chart == nil {
		return fmt.Errorf("profile %s names no chart to install %s from", in.ProfileName, in.App)
	}

	sensitive := r.sensitiveValues()
	if es := r.sensitiveSecret(sensitive); es != nil {
		out.add(es)
	}
	supplied, err := r.suppliedCredentials()
	if err != nil {
		return err
	}
	out.add(supplied...)

	values, err := r.inlineValues()
	if err != nil {
		return err
	}
	out.add(r.keepVolumes(), r.release(sensitive, values))
	// A chart the platform places names its own Services, and its routes
	// name them: it was never given an alias.
	if r.in.Placed {
		return nil
	}
	objs, observe := r.serviceAliases(values)
	out.add(objs...)
	out.ObserveOnly = append(out.ObserveOnly, observe...)
	return nil
}

// valuesRender holds what every part of the values render reads.
type valuesRender struct {
	in     *Input
	spec   *gentianov1alpha1.ComponentProfileSpec
	vm     *gentianov1alpha1.ValueMapping
	tenant string
	ns     string
}

// secretName is the Secret, and its ExternalSecret, the app's mapped values
// are read from.
func (r *valuesRender) secretName() string {
	return r.in.App + "-sensitive-values"
}

// appPath is where the tenant's store holds one category of the app's
// values.
func (r *valuesRender) appPath(category string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/apps/%s/%s", r.tenant, r.in.App, category)
}

func (r *valuesRender) generated() []gentianov1alpha1.AppSecret {
	if r.spec.Secrets == nil {
		return nil
	}
	return r.spec.Secrets.Generated
}

func (r *valuesRender) supplied() []gentianov1alpha1.SuppliedSecret {
	if r.spec.Secrets == nil {
		return nil
	}
	return r.spec.Secrets.Supplied
}

func (r *valuesRender) services() *gentianov1alpha1.ServiceRequirements {
	if r.spec.Requires == nil || r.spec.Requires.Services == nil {
		return &gentianov1alpha1.ServiceRequirements{}
	}
	return r.spec.Requires.Services
}

// valuesObject builds an object of the given kind and name.
func valuesObject(apiVersion, kind, name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetName(name)
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	return obj
}
