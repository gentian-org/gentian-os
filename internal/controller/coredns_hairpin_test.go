package controller

import (
	"strings"
	"testing"
)

func TestPatchHairpinCorefile_UpdatesKernelHostsPreservesMail(t *testing.T) {
	t.Parallel()

	corefile := `.:53 {
    hosts {
      # BEGIN gentian-hairpin
          10.152.183.197 desk.gentian.org
          10.152.183.197 id.desk.gentian.org
          10.152.183.139 mail.desk.gentian.org
          # END gentian-hairpin
          fallthrough
    }
}`

	patched, changed := patchHairpinCorefile(corefile, "10.152.183.36", "desk.gentian.org")
	if !changed {
		t.Fatal("expected hairpin patch to change Corefile")
	}
	if strings.Contains(patched, "10.152.183.197") {
		t.Fatalf("expected legacy ingress IP to be replaced, got:\n%s", patched)
	}
	if !strings.Contains(patched, "10.152.183.36 id.desk.gentian.org") {
		t.Fatalf("expected id host to point at Envoy IP, got:\n%s", patched)
	}
	if !strings.Contains(patched, "10.152.183.139 mail.desk.gentian.org") {
		t.Fatalf("expected mail host to be preserved, got:\n%s", patched)
	}
}

func TestPatchHairpinCorefile_Idempotent(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(hairpinBeginMarker + "\n")
	for _, host := range sortedHairpinHosts("desk.gentian.org") {
		b.WriteString("          10.152.183.36 ")
		b.WriteString(host)
		b.WriteByte('\n')
	}
	b.WriteString("          " + hairpinEndMarker)
	corefile := b.String()

	_, changed := patchHairpinCorefile(corefile, "10.152.183.36", "desk.gentian.org")
	if changed {
		t.Fatal("expected no change when hairpin already correct for managed hosts")
	}
}

func TestPatchHairpinCorefile_InsertsMissingHosts(t *testing.T) {
	t.Parallel()

	corefile := `# BEGIN gentian-hairpin
          10.152.183.197 portal.desk.gentian.org
          # END gentian-hairpin`

	patched, changed := patchHairpinCorefile(corefile, "10.152.183.36", "desk.gentian.org")
	if !changed {
		t.Fatal("expected missing kernel hosts to be inserted")
	}
	if !strings.Contains(patched, "10.152.183.36 id.desk.gentian.org") {
		t.Fatalf("expected missing id host to be added, got:\n%s", patched)
	}
}
