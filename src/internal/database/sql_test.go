package database

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestValidateSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jxaccount.sql")
	raw := []byte("DROP TABLE IF EXISTS account; CREATE TABLE account (loginName varchar(32), password_hash varchar(32));")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	result, err := ValidateSQL(path, int64(len(raw)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if !result.HasDropTable {
		t.Fatal("DROP TABLE was not detected")
	}
	if !reflect.DeepEqual(result.ExpectedTables, []string{"account"}) {
		t.Fatalf("expected tables = %v", result.ExpectedTables)
	}
}

func TestValidateSQLRejectsPrivilegeChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.sql")
	raw := []byte("CREATE TABLE account (loginName varchar(32), password_hash varchar(32)); GRANT ALL ON *.* TO root;")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if _, err := ValidateSQL(path, int64(len(raw)), hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("privilege-changing SQL was accepted")
	}
}

func TestValidateSQLRejectsDatabaseSwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad-use.sql")
	raw := []byte("USE jxaccount; CREATE TABLE account (loginName varchar(32), password_hash varchar(32));")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if _, err := ValidateSQL(path, int64(len(raw)), hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("database-switching SQL was accepted")
	}
}

func TestValidateSQLRejectsGlobalServerMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad-global.sql")
	raw := []byte("SET GLOBAL max_connections=1000; CREATE TABLE account (loginName varchar(32), password_hash varchar(32));")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if _, err := ValidateSQL(path, int64(len(raw)), hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("global server mutation was accepted")
	}
}

func TestValidateSQLRejectsExecutableCommentsAndClientCommands(t *testing.T) {
	cases := map[string]string{
		"mysql executable comment":   "/*!40101 SET GLOBAL max_connections=1000 */;\n",
		"mariadb executable comment": "/*M!100100 SET GLOBAL max_connections=1000 */;\n",
		"shell escape":               "\\! whoami\n",
		"source file":                "SOURCE C:/Windows/win.ini;\n",
		"delimiter":                  "DELIMITER $$\n",
		"system":                     "SYSTEM whoami\n",
		"bom and quit":               "\ufeffQUIT;\n",
	}
	const schema = "CREATE TABLE account (loginName varchar(32), password_hash varchar(32));\n"
	for name, prefix := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad-client-command.sql")
			raw := []byte(prefix + schema)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			if _, err := ValidateSQL(path, int64(len(raw)), hex.EncodeToString(sum[:])); err == nil {
				t.Fatal("executable SQL/client command was accepted")
			}
		})
	}
}

func TestValidateSQLAllowsOrdinaryNavicatBlockComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jxaccount.sql")
	raw := []byte("/*\nNavicat MySQL Data Transfer\nSource Database: jxaccount\n*/\n" +
		"CREATE TABLE account (loginName varchar(32), password_hash varchar(32));\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if _, err := ValidateSQL(path, int64(len(raw)), hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("ordinary block comment was rejected: %v", err)
	}
}
