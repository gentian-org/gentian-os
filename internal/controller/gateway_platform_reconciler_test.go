package controller

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKernelIngressSupersededByGateway(t *testing.T) {
	t.Parallel()
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gentian-portal-gentian-portal-web",
			Namespace: servicesNamespace,
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "portal.desk.gentian.org"}},
		},
	}
	if !kernelIngressSupersededByGateway(ing, "desk.gentian.org") {
		t.Fatal("expected kernel portal ingress to be superseded in gateway mode")
	}
	if kernelIngressSupersededByGateway(ing, "") {
		t.Fatal("expected skip when kernel domain unset")
	}
	other := ing.DeepCopy()
	other.Spec.Rules[0].Host = "example.com"
	if kernelIngressSupersededByGateway(other, "desk.gentian.org") {
		t.Fatal("expected unrelated host to be preserved")
	}
}
