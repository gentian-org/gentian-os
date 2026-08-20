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

package authz

import (
	"fmt"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	installedAppType = "installed_app"
	appContractType  = "app_contract"
	capabilityType   = "capability"
)

// InstalledAppObjectID formats tenant--profile for installed_app tuples.
func InstalledAppObjectID(tenant, app string) string {
	return fmt.Sprintf("%s--%s", tenant, app)
}

// AppContractObjectID formats tenant--consumer--contract for app_contract tuples.
func AppContractObjectID(tenant, consumer, contract string) string {
	return fmt.Sprintf("%s--%s--%s", tenant, consumer, contract)
}

// CapabilityObjectID formats tenant--consumer--contract--capability.
func CapabilityObjectID(tenant, consumer, contract, capability string) string {
	return fmt.Sprintf("%s--%s--%s--%s", tenant, consumer, contract, sanitizeCapability(capability))
}

func sanitizeCapability(cap string) string {
	return strings.NewReplacer(":", "-", "/", "-").Replace(cap)
}

// GrantTuples builds OpenFGA relationship tuples for an AppGrant.
func GrantTuples(tenant string, grant *gentianov1alpha1.AppGrant) []Tuple {
	if grant == nil {
		return nil
	}
	tenantObj := ObjectRef("tenant", tenant)
	providerApp := ObjectRef(installedAppType, InstalledAppObjectID(tenant, grant.Spec.App))
	var tuples []Tuple

	tuples = append(tuples, Tuple{
		User:     tenantObj,
		Relation: "tenant",
		Object:   providerApp,
	})

	for _, consume := range grant.Spec.Consume {
		contractObj := ObjectRef(appContractType, AppContractObjectID(tenant, grant.Spec.App, consume.Contract))
		consumerApp := ObjectRef(installedAppType, InstalledAppObjectID(tenant, grant.Spec.App))
		tuples = append(tuples,
			Tuple{User: tenantObj, Relation: "tenant", Object: contractObj},
			Tuple{User: consumerApp, Relation: "consumer", Object: contractObj},
		)
		for _, cap := range consume.Granted {
			capObj := ObjectRef(capabilityType, CapabilityObjectID(tenant, grant.Spec.App, consume.Contract, cap))
			tuples = append(tuples,
				Tuple{User: contractObj, Relation: "link", Object: capObj},
				Tuple{User: consumerApp, Relation: "granted", Object: capObj},
			)
		}
	}

	for _, allow := range grant.Spec.AllowConsumers {
		contractObj := ObjectRef(appContractType, AppContractObjectID(tenant, allow.App, allow.Contract))
		provider := ObjectRef(installedAppType, InstalledAppObjectID(tenant, grant.Spec.App))
		consumer := ObjectRef(installedAppType, InstalledAppObjectID(tenant, allow.App))
		tuples = append(tuples,
			Tuple{User: tenantObj, Relation: "tenant", Object: contractObj},
			Tuple{User: provider, Relation: "provider", Object: contractObj},
			Tuple{User: consumer, Relation: "consumer", Object: contractObj},
		)
		for _, cap := range allow.Scope {
			capObj := ObjectRef(capabilityType, CapabilityObjectID(tenant, allow.App, allow.Contract, cap))
			tuples = append(tuples,
				Tuple{User: contractObj, Relation: "link", Object: capObj},
				Tuple{User: consumer, Relation: "granted", Object: capObj},
			)
		}
	}
	return tuples
}

// GrantTupleKeys returns tuple keys owned by a grant for deletion on update.
func GrantTupleKeys(tenant string, grant *gentianov1alpha1.AppGrant) []TupleKey {
	tuples := GrantTuples(tenant, grant)
	return TupleKeysFromTuples(tuples)
}

// TupleKeysFromTuples extracts tuple keys from write tuples.
func TupleKeysFromTuples(tuples []Tuple) []TupleKey {
	keys := make([]TupleKey, 0, len(tuples))
	for _, t := range tuples {
		keys = append(keys, TupleKey{
			User:     t.User,
			Relation: t.Relation,
			Object:   t.Object,
		})
	}
	return keys
}

// TupleKeysNotIn returns keys present in a but absent from b.
func TupleKeysNotIn(a, b []TupleKey) []TupleKey {
	if len(a) == 0 {
		return nil
	}
	seen := tupleKeySet(b)
	out := make([]TupleKey, 0, len(a))
	for _, k := range a {
		if _, ok := seen[tupleKeyID(k)]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// TuplesMatchingKeys returns tuples whose keys appear in keys.
func TuplesMatchingKeys(all []Tuple, keys []TupleKey) []Tuple {
	if len(keys) == 0 {
		return nil
	}
	want := tupleKeySet(keys)
	out := make([]Tuple, 0, len(keys))
	for _, t := range all {
		k := TupleKey{User: t.User, Relation: t.Relation, Object: t.Object}
		if _, ok := want[tupleKeyID(k)]; ok {
			out = append(out, t)
		}
	}
	return out
}

func tupleKeySet(keys []TupleKey) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[tupleKeyID(k)] = struct{}{}
	}
	return set
}

func tupleKeyID(k TupleKey) string {
	return k.User + "\x00" + k.Relation + "\x00" + k.Object
}
