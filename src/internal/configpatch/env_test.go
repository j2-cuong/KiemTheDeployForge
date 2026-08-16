package configpatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLANRulesCoverEveryGameServer(t *testing.T) {
	rules := LANRules("10.0.0.5")
	if len(rules) != GameServerCount*2+3 {
		t.Fatalf("rule count = %d", len(rules))
	}
	for i := 1; i <= GameServerCount; i++ {
		for _, key := range []string{"InIp", "OutIp"} {
			want := Rule{ServerTargetRoot + "/Gameserver/GS" + itoa(i) + "servercfg.ini", "GameServer", key, "10.0.0.5"}
			if !containsRule(rules, want) {
				t.Fatalf("missing rule %+v", want)
			}
		}
	}
	for _, rule := range rules {
		if strings.HasPrefix(rule.RelativePath, "Sever/") {
			t.Fatalf("rule still targets the old Sever root: %s", rule.RelativePath)
		}
	}
}

func itoa(value int) string {
	if value < 10 {
		return string(rune('0' + value))
	}
	panic("unexpected GameServer index")
}

func containsRule(rules []Rule, want Rule) bool {
	for _, rule := range rules {
		if rule == want {
			return true
		}
	}
	return false
}

func TestPatchEnvActivatesCommentedAssignment(t *testing.T) {
	path := writeTempFile(t, "loginprobe.env",
		"BOT_DB_USER=old\r\n#BOT_GAMESERVER_DIR=D:\\old\\Gameserver\r\nBOT_DB_PORT=3306\r\n")
	if err := PatchEnv(path, "BOT_GAMESERVER_DIR", `C:\KiemTheServer\Server\Gameserver`); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := "BOT_DB_USER=old\r\nBOT_GAMESERVER_DIR=C:\\KiemTheServer\\Server\\Gameserver\r\nBOT_DB_PORT=3306\r\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPatchEnvRewritesActiveAssignmentAndKeepsCommentedOne(t *testing.T) {
	path := writeTempFile(t, "loginprobe.env",
		"#BOT_DB_USER=sample\nBOT_DB_USER=old\n")
	if err := PatchEnv(path, "BOT_DB_USER", "bot_writer"); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, path), "#BOT_DB_USER=sample\nBOT_DB_USER=bot_writer\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPatchEnvAppendsMissingKeyUsingFileLineEnding(t *testing.T) {
	path := writeTempFile(t, "loginprobe.env", "BOT_DB_PORT=3306\r\n")
	if err := PatchEnv(path, "BOT_DB_NAME", "jxaccount"); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, path), "BOT_DB_PORT=3306\r\nBOT_DB_NAME=jxaccount\r\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPatchEnvRejectsDuplicateActiveAssignment(t *testing.T) {
	path := writeTempFile(t, "loginprobe.env", "BOT_DB_USER=a\nBOT_DB_USER=b\n")
	if err := PatchEnv(path, "BOT_DB_USER", "bot_writer"); err == nil {
		t.Fatal("duplicate assignment was accepted")
	}
	if got, want := readFile(t, path), "BOT_DB_USER=a\nBOT_DB_USER=b\n"; got != want {
		t.Fatalf("file was modified: %q", got)
	}
}

func TestPatchEnvIsIdempotent(t *testing.T) {
	path := writeTempFile(t, "loginprobe.env", "#BOT_GAMESERVER_DIR=D:\\old\r\n")
	for range 2 {
		if err := PatchEnv(path, "BOT_GAMESERVER_DIR", `C:\KiemTheServer\Server\Gameserver`); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := readFile(t, path), "BOT_GAMESERVER_DIR=C:\\KiemTheServer\\Server\\Gameserver\r\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestApplyAndVerifyEnvBindBotToInstalledServer(t *testing.T) {
	root := t.TempDir()
	botDir := filepath.Join(root, "Bot")
	if err := os.MkdirAll(botDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(botDir, "loginprobe.env")
	original := "BOT_DB_HOST=10.0.0.9\r\nBOT_DB_USER=someone\r\n#BOT_GAMESERVER_DIR=D:\\elsewhere\\Gameserver\r\n"
	if err := os.WriteFile(envPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	installRoot := `C:\KiemTheServer`
	rules := BotEnvRules(installRoot, "bot_writer", "1234")
	if err := VerifyEnv(root, rules); err == nil {
		t.Fatal("unpatched env passed verification")
	}
	backup := filepath.Join(root, "backup")
	if err := ApplyEnv(root, rules, backup); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnv(root, rules); err != nil {
		t.Fatal(err)
	}
	patched := readFile(t, envPath)
	if !strings.Contains(patched, `BOT_GAMESERVER_DIR=C:\KiemTheServer\Server\Gameserver`) {
		t.Fatalf("bot is not bound to the installed server: %q", patched)
	}
	if !strings.Contains(patched, "BOT_DB_USER=bot_writer") || !strings.Contains(patched, "BOT_DB_PASSWORD=1234") {
		t.Fatalf("bot MySQL account was not applied: %q", patched)
	}
	if got := readFile(t, filepath.Join(backup, "Bot", "loginprobe.env")); got != original {
		t.Fatalf("backup does not hold the original: %q", got)
	}
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
