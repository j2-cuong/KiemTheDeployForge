package install

import (
	"path/filepath"
	"strings"
	"testing"
)

// Building the release into E:\test and then installing into E:\test used to
// take the resume path and fail with a missing install-state.json, which read
// like an internal bug rather than a wrong choice of directory.
func TestValidateInstallTargetRejectsTheMediaDirectory(t *testing.T) {
	media := t.TempDir()
	setup := filepath.Join(media, "Setup.exe")
	err := ValidateInstallTarget(media, setup)
	if err == nil {
		t.Fatal("installing into the directory that holds Setup.exe was accepted")
	}
	if !strings.Contains(err.Error(), media) {
		t.Fatalf("error does not name the offending directory: %v", err)
	}
}

// The install root must not swallow the media even when the media sits deeper.
func TestValidateInstallTargetRejectsAnAncestorOfTheMedia(t *testing.T) {
	root := t.TempDir()
	setup := filepath.Join(root, "release", "iso", "Setup.exe")
	if err := ValidateInstallTarget(root, setup); err == nil {
		t.Fatal("installing into an ancestor of the media directory was accepted")
	}
}

// Installing into a subdirectory of the media folder is a normal choice and
// must keep working: the media stays outside the directory being created.
func TestValidateInstallTargetAllowsSubdirectoryOfTheMedia(t *testing.T) {
	media := t.TempDir()
	setup := filepath.Join(media, "Setup.exe")
	target := filepath.Join(media, "KiemTheServer")
	if err := ValidateInstallTarget(target, setup); err != nil {
		t.Fatalf("a subdirectory of the media folder was rejected: %v", err)
	}
}

func TestValidateInstallTargetAllowsAnUnrelatedDirectory(t *testing.T) {
	setup := filepath.Join(t.TempDir(), "Setup.exe")
	target := filepath.Join(t.TempDir(), "KiemTheServer")
	if err := ValidateInstallTarget(target, setup); err != nil {
		t.Fatalf("an unrelated directory was rejected: %v", err)
	}
}

func TestValidateInstallTargetStillRejectsUnsafeRoots(t *testing.T) {
	setup := filepath.Join(t.TempDir(), "Setup.exe")
	for _, unsafe := range []string{"", `C:\`, `\\server\share`} {
		if err := ValidateInstallTarget(unsafe, setup); err == nil {
			t.Fatalf("unsafe install root %q was accepted", unsafe)
		}
	}
}

func TestPathContainsTreatsIdentityAndDescent(t *testing.T) {
	root := `C:\test`
	cases := []struct {
		candidate string
		want      bool
	}{
		{`C:\test`, true},
		{`C:\test\sub`, true},
		{`C:\test\sub\deeper`, true},
		{`C:\other`, false},
		{`C:\`, false},
		// A sibling whose name merely starts with the root must not match.
		{`C:\testing`, false},
	}
	for _, testCase := range cases {
		if got := pathContains(root, testCase.candidate); got != testCase.want {
			t.Fatalf("pathContains(%q, %q) = %v, want %v", root, testCase.candidate, got, testCase.want)
		}
	}
}
