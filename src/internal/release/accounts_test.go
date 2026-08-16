package release

import (
	"strings"
	"testing"
)

func TestWithDefaultsFillsBlanksOnly(t *testing.T) {
	got := Accounts{BotUser: "helper"}.WithDefaults()
	want := Accounts{RootPassword: DefaultRootPassword, BotUser: "helper", BotPassword: DefaultBotPassword}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if defaults := (Accounts{}).WithDefaults(); defaults != DefaultAccounts() {
		t.Fatalf("a blank struct did not resolve to the defaults: %+v", defaults)
	}
}

func TestDefaultAccountsAreValid(t *testing.T) {
	if err := DefaultAccounts().Validate(); err != nil {
		t.Fatalf("the stock credentials are rejected: %v", err)
	}
}

// sqlLiteral only doubles single quotes and MySQL treats a backslash as an
// escape character, so these characters must never reach a statement.
func TestValidateRejectsCharactersThatBreakSQLQuoting(t *testing.T) {
	for _, password := range []string{`pa'ss`, `pa"ss`, "pa`ss", `pa\ss`, "pa ss", "pa\tss", "pa\nss"} {
		accounts := DefaultAccounts()
		accounts.RootPassword = password
		if err := accounts.Validate(); err == nil {
			t.Fatalf("root password %q was accepted", password)
		}
		accounts = DefaultAccounts()
		accounts.BotPassword = password
		if err := accounts.Validate(); err == nil {
			t.Fatalf("bot password %q was accepted", password)
		}
	}
}

func TestValidateRejectsOverlongSecrets(t *testing.T) {
	accounts := DefaultAccounts()
	accounts.RootPassword = strings.Repeat("a", 33)
	if err := accounts.Validate(); err == nil {
		t.Fatal("a 33 character password was accepted")
	}
}

func TestValidateBotUserName(t *testing.T) {
	rejected := []string{
		"",                      // required
		"bot writer",            // space
		"bot-writer",            // hyphen is not a MySQL friendly identifier here
		"1bot",                  // starts with a digit
		"root",                  // reserved superuser
		"ROOT",                  // reserved regardless of case
		"ktf_helper",            // collides with temporary import accounts
		strings.Repeat("a", 17), // MySQL 5.5 truncates past 16 characters
	}
	for _, name := range rejected {
		accounts := DefaultAccounts()
		accounts.BotUser = name
		if err := accounts.Validate(); err == nil {
			t.Fatalf("bot user %q was accepted", name)
		}
	}
	for _, name := range []string{"bot_writer", "_helper", "Bot2", strings.Repeat("a", 16)} {
		accounts := DefaultAccounts()
		accounts.BotUser = name
		if err := accounts.Validate(); err != nil {
			t.Fatalf("bot user %q was rejected: %v", name, err)
		}
	}
}
