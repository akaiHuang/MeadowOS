package buildinfo

import (
	"strings"
	"testing"
)

func TestNativeProductIdentity(t *testing.T) {
	if got, want := Label(), "Meadow cli H_Go_V.0.1"; got != want {
		t.Fatalf("Label() = %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(Label()), "hermes") {
		t.Fatalf("native product label leaked the compatibility runtime: %q", Label())
	}
}

func TestDetailedLabelIncludesReleaseMetadata(t *testing.T) {
	oldCommit, oldBuildDate := Commit, BuildDate
	t.Cleanup(func() {
		Commit, BuildDate = oldCommit, oldBuildDate
	})
	Commit, BuildDate = "abc123", "2026-07-23"

	got := DetailedLabel()
	for _, want := range []string{Label(), "commit abc123", "built 2026-07-23"} {
		if !strings.Contains(got, want) {
			t.Fatalf("DetailedLabel() = %q, missing %q", got, want)
		}
	}
}

func TestDesktopAboutLabel(t *testing.T) {
	oldName, oldVersion, oldBuild := DesktopProductName, DesktopVersion, DesktopBuild
	t.Cleanup(func() {
		DesktopProductName, DesktopVersion, DesktopBuild = oldName, oldVersion, oldBuild
	})
	DesktopProductName = "Meadow Desktop"
	DesktopVersion = "0.1"
	DesktopBuild = "0.1"

	if got, want := DesktopAboutLabel(), "Meadow Desktop v0.1(0.1)"; got != want {
		t.Fatalf("DesktopAboutLabel() = %q, want %q", got, want)
	}
}

func TestCoreBuildIdentityTracksPackagedRebuilds(t *testing.T) {
	oldBuild, oldCommit, oldDate := DesktopBuild, Commit, BuildDate
	t.Cleanup(func() { DesktopBuild, Commit, BuildDate = oldBuild, oldCommit, oldDate })
	DesktopBuild, Commit, BuildDate = "test.1", "abc123", ""
	if CoreBuildIdentity() != "" {
		t.Fatal("unpackaged build must not claim a Core identity")
	}
	BuildDate = "2026-09-05T04:00:00Z"
	first := CoreBuildIdentity()
	if !strings.HasPrefix(first, "sha256:") || first != CoreBuildIdentity() {
		t.Fatal("packaged identity must be stable for the same artifact")
	}
	BuildDate = "2026-09-05T04:01:00Z"
	if first == CoreBuildIdentity() {
		t.Fatal("rebuild at the same commit must have a distinct identity")
	}
	second := CoreBuildIdentity()
	Commit = "def456"
	if second == CoreBuildIdentity() {
		t.Fatal("commit must contribute to identity")
	}
	third := CoreBuildIdentity()
	DesktopBuild = "test.2"
	if third == CoreBuildIdentity() {
		t.Fatal("Desktop build must contribute to identity")
	}
}

func TestDesktopBundledModels(t *testing.T) {
	old := DesktopBundledModelIDs
	t.Cleanup(func() { DesktopBundledModelIDs = old })
	DesktopBundledModelIDs = "Meadow 30B, Meadow 30B"
	if !DesktopHasBundledModels() {
		t.Fatal("expected a self-contained desktop build")
	}
	models := DesktopBundledModels()
	if len(models) != 1 || models[0] != "Meadow 30B" {
		t.Fatalf("bundled models = %#v", models)
	}
}
