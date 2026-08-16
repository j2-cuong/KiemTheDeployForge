package database

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractMySQLPackageRepairsInstallerPartialRuntime(t *testing.T) {
	root := t.TempDir()
	packagePath := filepath.Join(root, "mysql.zip")
	writeTestMySQLArchive(t, packagePath)
	runtimeRoot := filepath.Join(root, "Runtime", "MySQL55")
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeRoot, "bin", "mysqld.exe"), []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := extractMySQLPackage(packagePath, runtimeRoot, filepath.Join(runtimeRoot, "data-template"), true); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		filepath.Join("bin", "mysqld.exe"),
		filepath.Join("bin", "mysql.exe"),
		filepath.Join("data-template", "mysql", "user.frm"),
		runtimeMarkerName,
	} {
		if info, err := os.Stat(filepath.Join(runtimeRoot, relative)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("missing repaired runtime file %s: %v", relative, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(runtimeRoot, "bin", "mysqld.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fake mysqld" {
		t.Fatalf("mysqld content = %q", got)
	}
}

func TestExtractMySQLPackagePreservesUnknownRuntime(t *testing.T) {
	root := t.TempDir()
	packagePath := filepath.Join(root, "mysql.zip")
	writeTestMySQLArchive(t, packagePath)
	runtimeRoot := filepath.Join(root, "Runtime", "MySQL55")
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(runtimeRoot, "customer-file.txt")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := extractMySQLPackage(packagePath, runtimeRoot, filepath.Join(runtimeRoot, "data-template"), true)
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected ownership refusal, got %v", err)
	}
	got, readErr := os.ReadFile(foreign)
	if readErr != nil || string(got) != "keep me" {
		t.Fatalf("foreign file changed: %q, %v", got, readErr)
	}
}

func TestCopyDataTemplateRepairsPartialCopyTransactionally(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "template")
	destination := filepath.Join(root, "Data", "MySQL")
	writeDataTemplate(t, source)
	if err := os.MkdirAll(filepath.Join(destination, "mysql"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "mysql", "user.frm"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyDataTemplate(source, destination); err != nil {
		t.Fatal(err)
	}
	ready, _, err := inspectDataDirectory(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("repaired data directory did not verify")
	}
	if _, err := os.Stat(filepath.Join(destination, dataMarkerName)); err != nil {
		t.Fatalf("ready marker is missing: %v", err)
	}
}

func TestCopyDataTemplateDoesNotOverwriteInitializedDatabase(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "template")
	destination := filepath.Join(root, "Data", "MySQL")
	writeDataTemplate(t, source)
	if err := copyTemplateTree(source, destination); err != nil {
		t.Fatal(err)
	}
	changedPath := filepath.Join(destination, "mysql", "user.MYD")
	if err := os.WriteFile(changedPath, []byte("secured accounts"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "ibdata1"), []byte("initialized"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyDataTemplate(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(changedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secured accounts" {
		t.Fatalf("initialized data was overwritten: %q", got)
	}
}

func TestServiceCommandOwnedRequiresExactExecutableAndDefaultsFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Install Root")
	mysqld := filepath.Join(root, "Runtime", "MySQL55", "bin", "mysqld.exe")
	ini := filepath.Join(root, "Runtime", "MySQL55", "my.ini")
	command := `"` + mysqld + `" --defaults-file="` + ini + `" KiemTheServer-MySQL`
	owned, err := serviceCommandOwned(command, mysqld, ini)
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("exact managed service command was rejected")
	}
	foreign := filepath.Join(root, "Runtime", "Other", "my.ini")
	owned, err = serviceCommandOwned(command, mysqld, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if owned {
		t.Fatal("foreign defaults file was accepted")
	}
}

func TestTableSetComparisonIsCaseInsensitiveAndExact(t *testing.T) {
	if !sameTables([]string{"PayLog", "ACCOUNT"}, []string{"account", "paylog"}) {
		t.Fatal("equivalent table sets did not match")
	}
	if sameTables([]string{"account", "extra"}, []string{"account"}) {
		t.Fatal("extra table was ignored by exact comparison")
	}
	if !containsTables([]string{"account", "extra"}, []string{"ACCOUNT"}) {
		t.Fatal("expected table subset was not found")
	}
}

func TestMySQLEnvironmentDoesNotInheritPassword(t *testing.T) {
	t.Setenv("MYSQL_PWD", "foreign-password")
	for _, item := range mysqlEnvironment(false) {
		name, _, _ := strings.Cut(item, "=")
		if strings.EqualFold(name, "MYSQL_PWD") {
			t.Fatal("blank-password probe inherited MYSQL_PWD")
		}
	}
	found := false
	for _, item := range mysqlEnvironment(true) {
		name, value, _ := strings.Cut(item, "=")
		if strings.EqualFold(name, "MYSQL_PWD") {
			found = value == activeAccounts.RootPassword
		}
	}
	if !found {
		t.Fatal("managed root password was not set explicitly")
	}
}

func TestSchemaProbeUsesTemporaryTableWithoutTransaction(t *testing.T) {
	query := schemaProbeQuery("jxaccount", "ktf_probe_0011223344556677")
	upper := strings.ToUpper(query)
	if !strings.Contains(upper, "CREATE TEMPORARY TABLE") || !strings.Contains(upper, "DROP TEMPORARY TABLE") {
		t.Fatalf("probe is not temporary and self-cleaning: %s", query)
	}
	if strings.Contains(upper, "START TRANSACTION") || strings.Contains(upper, "ROLLBACK") || strings.Contains(upper, "INSERT INTO `JXACCOUNT`.`ACCOUNT`") {
		t.Fatalf("probe depends on transactions or mutates the live account table: %s", query)
	}
}

func TestImportCredentialFitsMySQL55UserLimit(t *testing.T) {
	user, password, err := newImportCredential()
	if err != nil {
		t.Fatal(err)
	}
	if !validImportUser(user) || len(user) > 16 || len(password) < 32 {
		t.Fatalf("invalid restricted importer credential: user=%q passwordLength=%d", user, len(password))
	}
}

func writeTestMySQLArchive(t *testing.T, path string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entries := map[string]string{
		"mysql-5.5.15-win32/bin/mysqld.exe":           "fake mysqld",
		"mysql-5.5.15-win32/bin/mysql.exe":            "fake mysql",
		"mysql-5.5.15-win32/share/english/errmsg.sys": "messages",
		"mysql-5.5.15-win32/data/mysql/user.frm":      "user schema",
		"mysql-5.5.15-win32/data/mysql/user.MYD":      "root account",
	}
	for name, content := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetModTime(time.Unix(1_600_000_000, 0).UTC())
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeDataTemplate(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		filepath.Join("mysql", "user.frm"):            "user schema",
		filepath.Join("mysql", "user.MYD"):            "blank root",
		filepath.Join("performance_schema", "db.opt"): "options",
	}
	for relative, content := range files {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
