/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// HomeSyncProfile is the profile of the platform's home sync, which every
// tenant is given: the realm knows it as vouch-homesync.
const HomeSyncProfile = "homesync"

// +kubebuilder:validation:XValidation:rule="has(self.calendar) || has(self.contacts)",message="homeSync keeps calendar, contacts or both"
// HomeSyncSpec is what an app that keeps calendars and contacts of its own
// declares so that the platform's home sync keeps them equal to the tenant's
// home (gentian-os#1156): per kind, how the app keeps it. The app writes no
// sync code; a driver for its framework speaks its API.
type HomeSyncSpec struct {
	// Endpoint is where the app's API answers inside the tenant: its
	// Service, the port and the path the driver's calls go under.
	Endpoint ContractEndpoint `json:"endpoint"`

	// Calendar is how the app keeps calendar entries.
	// +optional
	Calendar *HomeSyncMapping `json:"calendar,omitempty"`

	// Contacts is how the app keeps contacts.
	// +optional
	Contacts *HomeSyncMapping `json:"contacts,omitempty"`
}

// HomeSyncMapping is how an app keeps one kind of data.
// +kubebuilder:validation:XValidation:rule="self.fields.size() > 0",message="a mapping names at least one field"
type HomeSyncMapping struct {
	// Driver is the driver of the app's framework.
	// +kubebuilder:validation:Enum=odoo
	Driver string `json:"driver"`

	// Model is the app's record type (for Odoo, the model: calendar.event).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Model string `json:"model"`

	// Fields maps a part of an entry to the app's field. The parts of a
	// calendar entry are summary, description, location, start, end,
	// startDate, endDate, allDay and rrule; of a contact name, email,
	// phone, mobile, organization, title, note, street, city, zip, country
	// and website.
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['summary', 'description', 'location', 'start', 'end', 'startDate', 'endDate', 'allDay', 'rrule', 'name', 'email', 'phone', 'mobile', 'organization', 'title', 'note', 'street', 'city', 'zip', 'country', 'website'])",message="a field is one of the parts of a calendar entry or a contact"
	Fields map[string]string `json:"fields"`

	// Filter selects a person's records, in the driver's form (for Odoo, a
	// domain); "$user" and "$partner" stand for the person in the app.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Filter *apiextensionsv1.JSON `json:"filter,omitempty"`

	// Defaults are written onto a record made from the home, in the
	// driver's form, with the same placeholders.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	Defaults map[string]apiextensionsv1.JSON `json:"defaults,omitempty"`
}
