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

package controller

// DovecotDeployedForTest exposes the Dovecot gate to the external test package.
// The predicate decides whether an entire provisioning path runs, and it fails
// silently when wrong, so it is worth asserting directly.
func (r *TenantReconciler) DovecotDeployedForTest() bool { return r.dovecotDeployed() }
