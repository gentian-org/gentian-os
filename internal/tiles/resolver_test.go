package tiles

import (
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestResolveIconKnown(t *testing.T) {
	uri, err := ResolveIcon("mail")
	if err != nil {
		t.Fatalf("ResolveIcon: %v", err)
	}
	if !strings.HasPrefix(uri, "data:image/svg+xml;base64,") {
		t.Fatalf("unexpected uri prefix: %s", uri[:32])
	}
}

func TestResolveIconUnknown(t *testing.T) {
	if _, err := ResolveIcon("not-a-real-icon"); err == nil {
		t.Fatal("expected error for unknown icon")
	}
}

func TestResolveLogoPriority(t *testing.T) {
	profileIcon, err := ResolveIcon("erp")
	if err != nil {
		t.Fatalf("ResolveIcon: %v", err)
	}
	mailIcon, err := ResolveIcon("mail")
	if err != nil {
		t.Fatalf("ResolveIcon: %v", err)
	}

	got, err := ResolveLogo(
		&gentianov1alpha1.TileSpec{Icon: "erp"},
		"legacy-profile",
		&gentianov1alpha1.TileSpec{Icon: "mail"},
		"legacy-portal",
	)
	if err != nil {
		t.Fatalf("ResolveLogo: %v", err)
	}
	if got != mailIcon {
		t.Fatalf("portal tile icon should win")
	}

	got, err = ResolveLogo(&gentianov1alpha1.TileSpec{Icon: "erp"}, "legacy-profile", nil, "")
	if err != nil {
		t.Fatalf("ResolveLogo: %v", err)
	}
	if got != profileIcon {
		t.Fatalf("profile tile icon expected")
	}

	got, err = ResolveLogo(nil, "data:image/svg+xml;base64,QUJD", nil, "")
	if err != nil {
		t.Fatalf("ResolveLogo: %v", err)
	}
	if got != "data:image/svg+xml;base64,QUJD" {
		t.Fatalf("legacy profile logo expected, got %q", got)
	}
}
