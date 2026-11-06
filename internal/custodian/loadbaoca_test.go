/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestLoadBaoCA covers where the custodian takes OpenBao's CA from: nil keeps
// the system roots, an empty CA verifies nothing.
func TestLoadBaoCA(t *testing.T) {
	t.Setenv("BAO_CACERT", "")
	t.Setenv("BAO_CA_SECRET", "openbao-tls")
	t.Setenv("BAO_CA_SECRET_NAMESPACE", "kernel-secrets")
	t.Setenv("BAO_ADDR", "https://openbao.kernel-secrets.svc:8200")
	secret := func(data map[string][]byte) client.Object {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "openbao-tls", Namespace: "kernel-secrets"}, Data: data}
	}
	for _, c := range []struct {
		name    string
		objects []client.Object
		want    []byte
	}{
		{"ca.crt", []client.Object{secret(map[string][]byte{"ca.crt": []byte("CA"), "tls.crt": []byte("LEAF")})}, []byte("CA")},
		{"tls.crt only", []client.Object{secret(map[string][]byte{"tls.crt": []byte("LEAF")})}, []byte("LEAF")},
		{"no certificate in the Secret", []client.Object{secret(nil)}, []byte{}},
		{"no Secret", nil, nil},
	} {
		reader := fake.NewClientBuilder().WithObjects(c.objects...).Build()
		got := loadBaoCA(reader)
		if (got == nil) != (c.want == nil) || string(got) != string(c.want) {
			t.Errorf("%s: loadBaoCA = %q (nil %v), want %q (nil %v)", c.name, got, got == nil, c.want, c.want == nil)
		}
	}
}
