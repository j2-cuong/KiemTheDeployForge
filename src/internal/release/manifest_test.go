package release

import (
	"strings"
	"testing"
)

func TestSafeJoinRejectsTraversal(t *testing.T) {
	for _, value := range []string{"../escape", "C:/absolute", "..\\escape"} {
		if _, err := SafeJoin(`D:\safe`, value); err == nil {
			t.Fatalf("expected rejection for %q", value)
		}
	}
}

func TestValidSHA256(t *testing.T) {
	if !validSHA256(strings.Repeat("a", 64)) {
		t.Fatal("valid digest rejected")
	}
	if validSHA256("not-a-digest") {
		t.Fatal("invalid digest accepted")
	}
}

// The bot is optional, so includesBot must agree with what the payload really
// carries. Either direction of disagreement would make Setup skip a bot it
// should configure, or hunt for one that was never packaged.
func TestValidateChecksIncludesBotAgainstPayload(t *testing.T) {
	withBot := func(m *Manifest) {
		m.Files = append(m.Files, FileEntry{
			Path: "payload/Bot/loginprobe.exe", Target: "Bot/loginprobe.exe",
			Size: 1, SHA256: strings.Repeat("c", 64), LastWriteTimeUTC: "2026-01-01T00:00:00Z",
		})
		m.PayloadBytes++
	}
	cases := []struct {
		name        string
		includesBot bool
		addBotFile  bool
		wantErr     bool
	}{
		{"no bot declared and none packaged", false, false, false},
		{"bot declared and packaged", true, true, false},
		{"bot declared but not packaged", true, false, true},
		{"bot packaged but not declared", false, true, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := minimalManifest()
			manifest.IncludesBot = testCase.includesBot
			if testCase.addBotFile {
				withBot(manifest)
			}
			err := manifest.Validate()
			if testCase.wantErr && err == nil {
				t.Fatal("mismatched includesBot was accepted")
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("valid manifest rejected: %v", err)
			}
		})
	}
}

func TestValidateRejectsInvalidAccountsButAllowsABlankBlock(t *testing.T) {
	manifest := minimalManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatalf("a manifest without an accounts block was rejected: %v", err)
	}
	manifest.Accounts = Accounts{BotUser: "root"}
	if err := manifest.Validate(); err == nil {
		t.Fatal("a bot user of root was accepted")
	}
}

// minimalManifest is the smallest manifest that passes every other rule, so a
// test can isolate the one rule it is about.
func minimalManifest() *Manifest {
	mysqlEntry := FileEntry{
		Path: "prerequisites/mysql-5.5.15-win32.zip", Target: "InstallerData/packages/mysql-5.5.15-win32.zip",
		Size: 139896749, SHA256: strings.Repeat("a", 64), LastWriteTimeUTC: "2026-01-01T00:00:00Z",
	}
	sqlEntry := FileEntry{
		Path: "database/jxaccount.sql", Target: "InstallerData/database/jxaccount.sql",
		Size: 10, SHA256: strings.Repeat("b", 64), LastWriteTimeUTC: "2026-01-01T00:00:00Z",
	}
	return &Manifest{
		FormatVersion: 1, Product: "KiemTheDeployForge", ReleaseID: "KiemTheServer-test",
		CreatedUTC:   "2026-01-01T00:00:00Z",
		PayloadBytes: mysqlEntry.Size + sqlEntry.Size,
		Files:        []FileEntry{mysqlEntry, sqlEntry},
		MySQL: MySQLArtifact{
			Version: "5.5.15-win32", Path: mysqlEntry.Path, Target: mysqlEntry.Target,
			Size: mysqlEntry.Size, SHA256: mysqlEntry.SHA256, MD5: strings.Repeat("d", 32),
		},
		Database: SQLArtifact{Path: sqlEntry.Path, Target: sqlEntry.Target, Size: sqlEntry.Size, SHA256: sqlEntry.SHA256},
	}
}
