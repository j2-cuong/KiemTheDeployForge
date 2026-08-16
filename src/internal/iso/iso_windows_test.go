package iso

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPowerShellSourceParses(t *testing.T) {
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$source=[Console]::In.ReadToEnd(); [ScriptBlock]::Create($source)|Out-Null`)
	command.Stdin = strings.NewReader(powerShellSource)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ISO PowerShell source does not parse: %v: %s", err, output)
	}
}

func TestPowerShellSetsLargeUDFCapacityBeforeAddingPayload(t *testing.T) {
	capacity := strings.Index(powerShellSource, "$image.FreeMediaBlocks=[int]$requiredMediaBlocks")
	addTree := strings.Index(powerShellSource, "$image.Root.AddTree($SourceDirectory,$false)")
	if capacity < 0 || addTree < 0 || capacity > addTree {
		t.Fatal("UDF media capacity must be raised before adding the payload tree")
	}
	if !strings.Contains(powerShellSource, "+65536") {
		t.Fatal("UDF media capacity must reserve metadata headroom")
	}
	if !strings.Contains(powerShellSource, "$ExpectedMediaSize/2048.0") {
		t.Fatal("UDF capacity must be based on the external payload, not Setup.exe size")
	}
}

func TestPowerShellVerifiesBootstrapPayloadAndManifest(t *testing.T) {
	for _, required := range []string{"Setup.exe", "Payload.ktpkg", "manifests\\release.json", "ISO payload hash mismatch"} {
		if !strings.Contains(powerShellSource, required) {
			t.Fatalf("ISO verification is missing %q", required)
		}
	}
}

func TestPowerShellPropagatesDismountFailure(t *testing.T) {
	if strings.Contains(powerShellSource, "Dismount-DiskImage -ImagePath $OutputIso -ErrorAction SilentlyContinue") {
		t.Fatal("ISO verification must not suppress dismount failures")
	}
	for _, required := range []string{"$dismountError", "Dismount-DiskImage -ImagePath $OutputIso -ErrorAction Stop", "ISO verification succeeded but dismount failed"} {
		if !strings.Contains(powerShellSource, required) {
			t.Fatalf("ISO cleanup propagation is missing %q", required)
		}
	}
}

func TestCleanupBuildISOAttemptsRemoveAndPreservesBothErrors(t *testing.T) {
	dismountErr := errors.New("dismount failed")
	removeErr := errors.New("remove failed")
	removed := false
	err := cleanupBuildISOWith(`D:\output\.release.building.iso`, func(string) error {
		return dismountErr
	}, func(string) error {
		removed = true
		return removeErr
	})
	if !removed {
		t.Fatal("cleanup did not attempt to remove the incomplete ISO")
	}
	if !errors.Is(err, dismountErr) || !errors.Is(err, removeErr) {
		t.Fatalf("cleanup error lost a cause: %v", err)
	}
}

func TestCleanupBuildISOIgnoresMissingTemporaryFile(t *testing.T) {
	err := cleanupBuildISOWith(`D:\output\.missing.building.iso`, func(string) error { return nil }, func(string) error {
		return os.ErrNotExist
	})
	if err != nil {
		t.Fatalf("cleanup missing file error = %v", err)
	}
}

func TestCleanupWorkRootRetriesTransientFailure(t *testing.T) {
	attempts := 0
	err := cleanupWorkRootWith(`D:\output\.iso-build-test`, func(string) error {
		attempts++
		if attempts < 3 {
			return errors.New("busy")
		}
		return nil
	}, func(time.Duration) {})
	if err != nil || attempts != 3 {
		t.Fatalf("cleanup result: attempts=%d error=%v", attempts, err)
	}
}
