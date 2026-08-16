package install

import (
	"os"
	"path/filepath"
	"testing"

	"kiemthedeployforge/internal/release"
	"kiemthedeployforge/internal/sfx"
)

func TestCleanupStaleStagesRemovesOwnedStageFromPriorRelease(t *testing.T) {
	installRoot := filepath.Join(t.TempDir(), "KiemTheServer")
	stage := installRoot + ".staging-old"
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(stage, ".kiemthedeployforge-staging.json")
	if err := writeJSONAtomic(marker, stagingOwner{Product: "KiemTheDeployForge", ReleaseID: "release-1", InstallRoot: installRoot}); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleStages(installRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("owned stale stage remains: %v", err)
	}
}

func TestCleanupStaleStagesPreservesUnknownDirectory(t *testing.T) {
	installRoot := filepath.Join(t.TempDir(), "KiemTheServer")
	stage := installRoot + ".staging-foreign"
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleStages(installRoot); err == nil {
		t.Fatal("foreign staging directory was accepted")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatalf("foreign staging directory was removed: %v", err)
	}
}

func TestPayloadRepairPolicyPreservesMutableConfigs(t *testing.T) {
	cases := []struct {
		target string
		want   sfx.RepairMode
	}{
		{"Client/user/uicommon.ini", sfx.RepairMissing},
		{"Client/pak/update.pak", sfx.RepairVerify},
		{"Server/Gameserver/log/gameserver.log", sfx.RepairMissing},
		// Every GameServer config Setup rewrites must stay restore-if-missing,
		// including the ones beyond GS4 that used to be verified against the
		// unpatched manifest bytes.
		{"Server/Gameserver/GS1servercfg.ini", sfx.RepairMissing},
		{"Server/Gameserver/GS9servercfg.ini", sfx.RepairMissing},
		// The bot env file is rewritten with machine specific paths.
		{"Bot/loginprobe.env", sfx.RepairMissing},
		{"Bot/loginprobe.exe", sfx.RepairVerify},
		// The old Sever root is no longer part of the payload.
		{"Sever/Gameserver/GS1servercfg.ini", sfx.RepairSkip},
	}
	for _, testCase := range cases {
		if mode := payloadRepairMode(release.FileEntry{Target: testCase.target}); mode != testCase.want {
			t.Fatalf("%s repair mode = %v, want %v", testCase.target, mode, testCase.want)
		}
	}
}

func TestBotDiskRequirementIsTwentyGiB(t *testing.T) {
	if BotSystemDriveFreeBytes != 20*1024*1024*1024 {
		t.Fatalf("bot system drive requirement = %d", BotSystemDriveFreeBytes)
	}
}
