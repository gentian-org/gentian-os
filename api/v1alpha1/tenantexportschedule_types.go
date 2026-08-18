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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantExportScheduleSpec takes exports on a schedule and expires old ones.
//
// A CronJob applying TenantExports would have worked, but it would need an
// image, a ServiceAccount and RBAC in every tenant namespace, and it would have
// nowhere to record what it had done. Reconciling the schedule keeps the whole
// thing in one place and makes "when did this last run, and did it work"
// answerable from the resource itself.
type TenantExportScheduleSpec struct {
	// Schedule is a five-field cron expression, interpreted in UTC.
	//
	// UTC and not the cluster's zone: a schedule that silently shifts by an
	// hour twice a year is a schedule nobody can reason about during an
	// incident.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`

	// Suspend stops new exports without deleting the schedule or its history.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Apps limits each export to these profiles. Empty captures everything.
	// +optional
	Apps []string `json:"apps,omitempty"`

	// Encryption for the exports this schedule creates. Defaults to the
	// cluster's recipients, which is the only mode that works unattended — a
	// passphrase has nobody to type it at 03:00.
	// +optional
	Encryption *ExportEncryption `json:"encryption,omitempty"`

	// KeepLast retains this many finished exports and deletes the rest.
	//
	// Retention has to be someone's job or bundles accumulate until the bucket
	// is the problem. Zero means keep everything, which is a decision rather
	// than a default: it is the right answer only when something else prunes.
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepLast int32 `json:"keepLast,omitempty"`
}

// TenantExportScheduleStatus reports what the schedule has done.
type TenantExportScheduleStatus struct {
	// LastScheduleTime is when an export was last created.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastExportName is the export created then, so the outcome is one lookup
	// away rather than a search through the namespace.
	// +optional
	LastExportName string `json:"lastExportName,omitempty"`

	// NextScheduleTime is when the next export is due.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`

	// LastSuccessfulTime is when an export from this schedule last reached
	// Ready. A schedule that is firing but never succeeding looks healthy by
	// every other measure, which is exactly the failure a backup regime cannot
	// afford; this is the field that gives it away.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// Conditions carry Ready, which is False when the schedule is unusable
	// (an invalid expression) rather than merely idle.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// TenantExportSchedule creates TenantExports on a cron schedule.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=texs
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Last-Success",type=date,JSONPath=`.status.lastSuccessfulTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TenantExportSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantExportScheduleSpec   `json:"spec,omitempty"`
	Status TenantExportScheduleStatus `json:"status,omitempty"`
}

// TenantExportScheduleList contains a list of TenantExportSchedule.
// +kubebuilder:object:root=true
type TenantExportScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantExportSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TenantExportSchedule{}, &TenantExportScheduleList{})
}
