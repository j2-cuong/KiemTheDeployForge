package database

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"kiemthedeployforge/internal/release"
	"kiemthedeployforge/internal/winfile"
	"kiemthedeployforge/internal/winprocess"
)

const (
	ServiceName = "KiemTheServer-MySQL"

	// RootUser is the fixed superuser name; only its password is configurable.
	RootUser = release.DefaultRootUser

	// DatabaseName is the single database Setup publishes and grants on.
	DatabaseName = release.DatabaseName

	PinnedMySQLName              = "mysql-5.5.15-win32.zip"
	PinnedMySQLURL               = "https://cdn.mysql.com/archives/mysql-5.5/mysql-5.5.15-win32.zip"
	PinnedMySQLSize              = int64(139896749)
	PinnedMySQLMD5               = "127cf3abe2fa31b58d91e45e65194f25"
	PinnedMySQLSHA256            = "976571c110a9441a26ccf936407a90376dd30f0adce4cc6870be17fcc5ed001e"
	runtimeMarkerName            = ".kiemthedeployforge-runtime-ready"
	dataMarkerName               = ".kiemthedeployforge-data-ready"
	importMarkerName             = "jxaccount-import-state.json"
	stagePrefix                  = "jxaccount_ktf_stage_"
	prepareMarkerName            = ".kiemthedeployforge-prepare"
	runtimePrepPrefix            = ".MySQL55.prepare-"
	dataPrepPrefix               = ".MySQL.prepare-"
	sqlImportExpansionMultiplier = uint64(4)
	sqlImportExpansionFloor      = uint64(512 * 1024 * 1024)
)

// activeAccounts holds the credentials of the release currently being
// installed.
//
// The mysql.exe helpers below take a passwordSet boolean rather than a
// password, and threading a credential through every one of them would touch
// far more code than it would clarify. Setup installs one release at a time
// behind a machine wide mutex and EnsureMySQL sets this before any helper runs,
// so a single ambient value is both sufficient and unambiguous.
var activeAccounts = release.DefaultAccounts()

type MySQLOptions struct {
	InstallRoot string
	SQLPath     string
	SQL         release.SQLArtifact
	Package     release.MySQLArtifact
	// Accounts are the credentials recorded in the release manifest. A zero
	// value falls back to the stock ones.
	Accounts              release.Accounts
	AdoptCompleteDatabase bool
	CheckImportSpace      func() error
	Logf                  func(string, ...any)
	Record                func(phase, resource, action, status string, owned bool, operationErr error)
}

type MySQLResult struct {
	Managed bool   `json:"managed"`
	Service string `json:"service"`
	Version string `json:"version"`
	// Adopted marks a MySQL that was already serving 127.0.0.1:3306 before this
	// installation. Setup completes its accounts and database in place instead
	// of installing a second server on the same port.
	Adopted          bool   `json:"adopted"`
	DatabaseImported bool   `json:"databaseImported"`
	DatabaseReady    bool   `json:"databaseReady"`
	Database         string `json:"database,omitempty"`
	RootAccount      string `json:"rootAccount,omitempty"`
	BotAccount       string `json:"botAccount,omitempty"`
}

type MySQLDiskEstimate struct {
	RuntimeBytes      uint64
	DataTemplateBytes uint64
	DatabaseBytes     uint64
	TotalBytes        uint64
}

func EstimateInstallDisk(packagePath string, sqlSize int64) (MySQLDiskEstimate, error) {
	if sqlSize < 0 {
		return MySQLDiskEstimate{}, fmt.Errorf("SQL size is negative")
	}
	archive, err := zip.OpenReader(packagePath)
	if err != nil {
		return MySQLDiskEstimate{}, fmt.Errorf("open pinned MySQL package for disk estimate: %w", err)
	}
	defer archive.Close()
	layout, err := buildRuntimeLayout(archive.File)
	if err != nil {
		return MySQLDiskEstimate{}, err
	}
	estimate := MySQLDiskEstimate{}
	for relative, entry := range layout.files {
		estimate.RuntimeBytes, err = checkedAddUint64(estimate.RuntimeBytes, entry.UncompressedSize64)
		if err != nil {
			return MySQLDiskEstimate{}, err
		}
		if strings.HasPrefix(strings.ToLower(filepath.ToSlash(relative)), "data-template/") {
			estimate.DataTemplateBytes, err = checkedAddUint64(estimate.DataTemplateBytes, entry.UncompressedSize64)
			if err != nil {
				return MySQLDiskEstimate{}, err
			}
		}
	}
	databaseBytes := uint64(sqlSize)
	if databaseBytes > math.MaxUint64/sqlImportExpansionMultiplier {
		return MySQLDiskEstimate{}, fmt.Errorf("SQL disk estimate overflow")
	}
	databaseBytes *= sqlImportExpansionMultiplier
	if databaseBytes < sqlImportExpansionFloor {
		databaseBytes = sqlImportExpansionFloor
	}
	estimate.DatabaseBytes = databaseBytes
	estimate.TotalBytes, err = checkedAddUint64(estimate.RuntimeBytes, estimate.DataTemplateBytes, estimate.DatabaseBytes)
	if err != nil {
		return MySQLDiskEstimate{}, err
	}
	return estimate, nil
}

func checkedAddUint64(values ...uint64) (uint64, error) {
	var total uint64
	for _, value := range values {
		if total > math.MaxUint64-value {
			return 0, fmt.Errorf("disk estimate overflow")
		}
		total += value
	}
	return total, nil
}

func EnsureMySQL(ctx context.Context, options MySQLOptions) (result MySQLResult, err error) {
	if !strings.EqualFold(options.Package.SHA256, PinnedMySQLSHA256) {
		return result, fmt.Errorf("release does not contain the pinned MySQL 5.5.15 package")
	}
	accounts := options.Accounts.WithDefaults()
	if err := accounts.Validate(); err != nil {
		return result, fmt.Errorf("release MySQL accounts are invalid: %w", err)
	}
	activeAccounts = accounts
	validation, err := ValidateSQL(options.SQLPath, options.SQL.Size, options.SQL.SHA256)
	if err != nil {
		return result, err
	}
	packagePath := filepath.Join(options.InstallRoot, filepath.FromSlash(options.Package.Target))
	if err := release.VerifyFile(packagePath, options.Package.Size, options.Package.SHA256); err != nil {
		return result, fmt.Errorf("verify pinned MySQL package: %w", err)
	}
	runtimeRoot := filepath.Join(options.InstallRoot, "Runtime", "MySQL55")
	dataTemplate := filepath.Join(runtimeRoot, "data-template")
	dataRoot := filepath.Join(options.InstallRoot, "Data", "MySQL")
	mysqld := filepath.Join(runtimeRoot, "bin", "mysqld.exe")
	mysql := filepath.Join(runtimeRoot, "bin", "mysql.exe")
	iniPath := filepath.Join(runtimeRoot, "my.ini")
	service, err := queryWindowsService(ServiceName)
	if err != nil {
		return result, err
	}
	// managedService means the service under our name really is the one this
	// installer registered, so it may be reconfigured and restarted. Anything
	// else is treated as a foreign server: Setup completes it in place instead
	// of replacing it.
	managedService := false
	if service.Exists {
		owned, ownErr := serviceCommandOwned(service.BinaryPath, mysqld, iniPath)
		if ownErr != nil {
			return result, fmt.Errorf("inspect the existing %s service: %w", ServiceName, ownErr)
		}
		managedService = owned
	}
	// The client binaries are extracted even when an existing server is
	// adopted, because mysql.exe is how Setup inspects and completes it.
	err = extractMySQLPackageContext(ctx, packagePath, runtimeRoot, dataTemplate, !managedService)
	record(options, "mysql-runtime", runtimeRoot, "prepare-transactionally", status(err), true, err)
	if err != nil {
		return result, fmt.Errorf("extract MySQL runtime: %w", err)
	}
	if managedService {
		if err := verifyMyINI(iniPath, runtimeRoot, dataRoot); err != nil {
			return result, fmt.Errorf("verify managed MySQL configuration: %w", err)
		}
	}
	versionOutput, err := commandOutput(ctx, mysqld, "--no-defaults", "--version")
	if err != nil || !strings.Contains(versionOutput, "5.5.15") || !strings.Contains(strings.ToLower(versionOutput), "win32") {
		return result, fmt.Errorf("extracted mysqld is not MySQL 5.5.15 Win32: %s", strings.TrimSpace(versionOutput))
	}

	if managedService && service.Running {
		return finishManagedMySQL(ctx, mysql, options, validation)
	}
	if !managedService && portOpen("127.0.0.1:3306") {
		return adoptExistingMySQL(ctx, mysql, options, validation)
	}
	if service.Exists && !managedService {
		return result, fmt.Errorf(
			"a Windows service named %s already exists, is not the one this installer registered, and is not serving 127.0.0.1:3306; "+
				"remove or rename that service, then run Setup again", ServiceName)
	}
	if err := copyDataTemplateContext(ctx, dataTemplate, dataRoot); err != nil {
		record(options, "mysql-data", dataRoot, "prepare-transactionally", "failed", true, err)
		return result, fmt.Errorf("prepare MySQL data directory: %w", err)
	}
	record(options, "mysql-data", dataRoot, "prepare-transactionally", "completed", true, nil)
	if err := writeMyINI(iniPath, runtimeRoot, dataRoot); err != nil {
		return result, err
	}
	// Only a service this installer registered is ever started here; a foreign
	// one was already handled above.
	if managedService {
		output, startErr := commandOutput(ctx, "sc.exe", "start", ServiceName)
		if startErr != nil && !strings.Contains(output, "1056") {
			return result, fmt.Errorf("resume MySQL service: %w (%s)", startErr, cleanOutput(output))
		}
		return finishManagedMySQL(ctx, mysql, options, validation)
	}
	if options.Logf != nil {
		options.Logf("Registering managed MySQL service %s", ServiceName)
	}
	output, err := commandOutput(ctx, mysqld, "--install", ServiceName, "--defaults-file="+iniPath)
	record(options, "mysql-register-service", "service:"+ServiceName, "create", status(err), true, err)
	if err != nil {
		return result, fmt.Errorf("register MySQL service: %w (%s)", err, cleanOutput(output))
	}
	_, _ = commandOutput(ctx, "sc.exe", "config", ServiceName, "start=", "auto")
	output, err = commandOutput(ctx, "sc.exe", "start", ServiceName)
	record(options, "mysql-start-service", "service:"+ServiceName, "start", status(err), true, err)
	if err != nil {
		return result, fmt.Errorf("start MySQL service: %w (%s)", err, cleanOutput(output))
	}
	return finishManagedMySQL(ctx, mysql, options, validation)
}

// adoptExistingMySQL completes a MySQL that was already serving
// 127.0.0.1:3306 before this installation.
//
// Refusing to continue was wrong for the common case: the operator already has
// the game's MySQL installed, so a second server cannot bind the port and there
// is nothing to fix by stopping. What actually matters is whether the accounts
// and the database this release needs are present, so Setup checks them and
// fills in only what is missing.
//
// A foreign server is treated as someone else's property. Unlike the managed
// path this never deletes anonymous accounts, never drops the test database and
// never rewrites a configuration file. The only writes are the ones the release
// genuinely requires: the bot account, and jxaccount when it is absent.
func adoptExistingMySQL(ctx context.Context, mysql string, options MySQLOptions, validation SQLValidation) (MySQLResult, error) {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	logf("MySQL is already serving 127.0.0.1:3306; checking whether its accounts and database can be reused")

	passwordSet, err := waitForEitherCredential(ctx, mysql, 20*time.Second)
	if err != nil {
		record(options, "mysql-adopt", "127.0.0.1:3306", "authenticate", "failed", false, err)
		return MySQLResult{}, fmt.Errorf(
			"127.0.0.1:3306 is already serving MySQL, but Setup could not sign in as %q using password %q or an empty password. "+
				"Either stop that MySQL so Setup can install its own service %s, or set its %s password to %q and run Setup again: %w",
			RootUser, activeAccounts.RootPassword, ServiceName, RootUser, activeAccounts.RootPassword, err)
	}
	if !passwordSet {
		// The release contract, the bot env file and the operator facing text
		// all state root/1234, so an unprotected root is aligned to it.
		logf("Existing MySQL accepts %s without a password; setting the documented password", RootUser)
		if _, err := mysqlCommand(ctx, mysql, false,
			"UPDATE mysql.user SET Password=PASSWORD('"+sqlLiteral(activeAccounts.RootPassword)+"') WHERE User='"+sqlLiteral(RootUser)+"';FLUSH PRIVILEGES;", nil); err != nil {
			return MySQLResult{}, fmt.Errorf("set the %s password on the existing MySQL: %w", RootUser, err)
		}
		if err := waitForMySQL(ctx, mysql, true, 30*time.Second); err != nil {
			return MySQLResult{}, fmt.Errorf("reconnect to the existing MySQL after setting the %s password: %w", RootUser, err)
		}
	}
	version, err := mysqlScalar(ctx, mysql, true, "SELECT VERSION()")
	if err != nil {
		return MySQLResult{}, fmt.Errorf("read the existing MySQL version: %w", err)
	}
	version = strings.TrimSpace(version)
	logf("Reusing the existing MySQL %s on 127.0.0.1:3306", version)
	record(options, "mysql-adopt", "127.0.0.1:3306", "authenticate", "completed", false, nil)

	if err := ensureBotAccount(ctx, mysql); err != nil {
		return MySQLResult{}, err
	}
	record(options, "mysql-bot-account", activeAccounts.BotUser+"@"+DatabaseName, "grant", "completed", false, nil)

	markerPath := filepath.Join(options.InstallRoot, "InstallerData", "database", importMarkerName)
	// An existing jxaccount whose tables already match this SQL artifact is
	// accepted as it is; the operator's live accounts are never re-imported.
	imported, err := ensureDatabase(ctx, mysql, options.SQLPath, options.SQL.SHA256, validation, markerPath, true, options.CheckImportSpace)
	if err != nil {
		return MySQLResult{}, err
	}
	if imported {
		logf("Imported %s into the existing MySQL", DatabaseName)
	} else {
		logf("%s already present in the existing MySQL; left unchanged", DatabaseName)
	}
	return MySQLResult{
		Managed: false, Adopted: true, Service: "", Version: version,
		DatabaseImported: imported, DatabaseReady: true,
		RootAccount: RootUser, BotAccount: activeAccounts.BotUser, Database: DatabaseName,
	}, nil
}

func finishManagedMySQL(ctx context.Context, mysql string, options MySQLOptions, validation SQLValidation) (MySQLResult, error) {
	passwordSet, err := waitForEitherCredential(ctx, mysql, 90*time.Second)
	if err != nil {
		return MySQLResult{}, err
	}
	if err := secureInitialAccounts(ctx, mysql, passwordSet); err != nil {
		return MySQLResult{}, err
	}
	version, err := mysqlScalar(ctx, mysql, true, "SELECT VERSION()")
	if err != nil {
		return MySQLResult{}, err
	}
	if !strings.HasPrefix(version, "5.5.15") {
		return MySQLResult{}, fmt.Errorf("managed MySQL version %q is not supported", version)
	}
	markerPath := filepath.Join(options.InstallRoot, "InstallerData", "database", importMarkerName)
	imported, err := ensureDatabase(ctx, mysql, options.SQLPath, options.SQL.SHA256, validation, markerPath, options.AdoptCompleteDatabase, options.CheckImportSpace)
	if err != nil {
		return MySQLResult{}, err
	}
	if err := ensureBotAccount(ctx, mysql); err != nil {
		return MySQLResult{}, err
	}
	record(options, "mysql-bot-account", activeAccounts.BotUser+"@"+DatabaseName, "grant", "completed", true, nil)
	return MySQLResult{
		Managed: true, Service: ServiceName, Version: version,
		DatabaseImported: imported, DatabaseReady: true,
		RootAccount: RootUser, BotAccount: activeAccounts.BotUser, Database: DatabaseName,
	}, nil
}

// ensureBotAccount publishes bot_writer with full rights on jxaccount. The bot
// issues CREATE TABLE IF NOT EXISTS for its own bot_runtime_state table, so a
// data-only grant is not sufficient. MySQL 5.5 has no DROP USER IF EXISTS, but
// GRANT ... IDENTIFIED BY creates the account or resets its password, which
// makes this safe to rerun on resume.
func ensureBotAccount(ctx context.Context, mysql string) error {
	grant := quoteIdentifier(DatabaseName) + ".* TO '" + sqlLiteral(activeAccounts.BotUser) + "'@"
	statements := "GRANT ALL PRIVILEGES ON " + grant + "'localhost' IDENTIFIED BY '" + sqlLiteral(activeAccounts.BotPassword) + "';" +
		"GRANT ALL PRIVILEGES ON " + grant + "'%' IDENTIFIED BY '" + sqlLiteral(activeAccounts.BotPassword) + "';" +
		"FLUSH PRIVILEGES;"
	if _, err := mysqlCommand(ctx, mysql, true, statements, nil); err != nil {
		return fmt.Errorf("create the %s MySQL account: %w", activeAccounts.BotUser, err)
	}
	accounts, err := mysqlScalar(ctx, mysql, true,
		"SELECT COUNT(*) FROM mysql.user WHERE User='"+sqlLiteral(activeAccounts.BotUser)+"' AND Host IN ('localhost','%') AND Password=PASSWORD('"+sqlLiteral(activeAccounts.BotPassword)+"')")
	if err != nil {
		return fmt.Errorf("verify the %s MySQL account: %w", activeAccounts.BotUser, err)
	}
	if strings.TrimSpace(accounts) != "2" {
		return fmt.Errorf("the %s MySQL account was not created for both localhost and %%", activeAccounts.BotUser)
	}
	grants, err := mysqlScalar(ctx, mysql, true,
		"SELECT COUNT(*) FROM mysql.db WHERE Db='"+sqlLiteral(DatabaseName)+"' AND User='"+sqlLiteral(activeAccounts.BotUser)+"'"+
			" AND Select_priv='Y' AND Insert_priv='Y' AND Update_priv='Y' AND Delete_priv='Y' AND Create_priv='Y'")
	if err != nil {
		return fmt.Errorf("verify the %s privileges on %s: %w", activeAccounts.BotUser, DatabaseName, err)
	}
	if strings.TrimSpace(grants) != "2" {
		return fmt.Errorf("the %s account does not have full privileges on %s", activeAccounts.BotUser, DatabaseName)
	}
	return nil
}

func extractMySQLPackage(packagePath, runtimeRoot, templateRoot string, allowRepair bool) error {
	return extractMySQLPackageContext(context.Background(), packagePath, runtimeRoot, templateRoot, allowRepair)
}

func extractMySQLPackageContext(ctx context.Context, packagePath, runtimeRoot, templateRoot string, allowRepair bool) error {
	if !strings.EqualFold(filepath.Clean(templateRoot), filepath.Join(filepath.Clean(runtimeRoot), "data-template")) {
		return fmt.Errorf("invalid MySQL data-template path")
	}
	archive, err := zip.OpenReader(packagePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	layout, err := buildRuntimeLayout(archive.File)
	if err != nil {
		return err
	}
	ready, repairable, err := inspectRuntime(ctx, runtimeRoot, layout)
	if err != nil {
		return err
	}
	if ready {
		return removeOwnedPreparationMarker(runtimeRoot, "mysql-runtime")
	}
	if !repairable {
		return fmt.Errorf("existing MySQL runtime contains files not owned by this installer: %s", runtimeRoot)
	}
	if _, err := os.Stat(runtimeRoot); err == nil && !allowRepair {
		return fmt.Errorf("MySQL runtime is incomplete while service %s exists; refusing to replace files in use", ServiceName)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(runtimeRoot)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if err := cleanupOwnedPreparationDirectories(parent, runtimePrepPrefix, "mysql-runtime"); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, runtimePrepPrefix)
	if err != nil {
		return err
	}
	keepStaging := true
	defer func() {
		if keepStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := markPreparationDirectory(staging, "mysql-runtime"); err != nil {
		return err
	}
	buffer := make([]byte, 4*1024*1024)
	for _, entry := range archive.File {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		relative, skip, err := runtimeRelativePath(entry.Name)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		destination, err := release.SafeJoin(staging, relative)
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			input.Close()
			return err
		}
		written, copyErr := copyWithContext(ctx, output, input, buffer)
		var syncErr error
		if copyErr == nil {
			syncErr = output.Sync()
		}
		closeErr := output.Close()
		inputCloseErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if written != int64(entry.UncompressedSize64) {
			return fmt.Errorf("unexpected extracted size for %s", entry.Name)
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		if inputCloseErr != nil {
			return inputCloseErr
		}
		if err := os.Chtimes(destination, entry.Modified, entry.Modified); err != nil {
			return err
		}
	}
	if err := validateRuntimeRequired(staging); err != nil {
		return err
	}
	if err := writeAtomicFile(filepath.Join(staging, runtimeMarkerName), []byte(runtimeMarkerContent()), 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(runtimeRoot); err == nil {
		if err := os.RemoveAll(runtimeRoot); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staging, runtimeRoot); err != nil {
		return err
	}
	keepStaging = false
	return removeOwnedPreparationMarker(runtimeRoot, "mysql-runtime")
}

type runtimeLayout struct {
	files       map[string]*zip.File
	directories map[string]struct{}
}

func buildRuntimeLayout(entries []*zip.File) (*runtimeLayout, error) {
	layout := &runtimeLayout{files: make(map[string]*zip.File), directories: make(map[string]struct{})}
	for _, entry := range entries {
		relative, skip, err := runtimeRelativePath(entry.Name)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		key := strings.ToLower(filepath.Clean(filepath.FromSlash(relative)))
		if entry.FileInfo().IsDir() {
			layout.directories[key] = struct{}{}
			continue
		}
		if _, exists := layout.files[key]; exists {
			return nil, fmt.Errorf("duplicate MySQL archive path %q", entry.Name)
		}
		layout.files[key] = entry
		for parent := filepath.Dir(key); parent != "."; parent = filepath.Dir(parent) {
			layout.directories[parent] = struct{}{}
		}
	}
	return layout, nil
}

func runtimeRelativePath(name string) (relative string, skip bool, err error) {
	const rootPrefix = "mysql-5.5.15-win32/"
	name = strings.ReplaceAll(name, "\\", "/")
	if !strings.HasPrefix(name, rootPrefix) {
		return "", false, fmt.Errorf("unexpected archive entry %q", name)
	}
	relative = strings.TrimPrefix(name, rootPrefix)
	if relative == "" {
		return "", true, nil
	}
	if strings.HasPrefix(relative, "data/") {
		relative = "data-template/" + strings.TrimPrefix(relative, "data/")
	}
	if strings.Trim(relative, "/") == "data-template" {
		return "", true, nil
	}
	if _, err := release.SafeJoin(".", relative); err != nil {
		return "", false, err
	}
	return relative, false, nil
}

func inspectRuntime(ctx context.Context, root string, layout *runtimeLayout) (ready, repairable bool, returnErr error) {
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, false, nil
	}
	complete, err := runtimeMatchesPinnedArchive(ctx, root, layout)
	if err != nil {
		return false, false, err
	}
	if complete {
		if err := validateRuntimeRequired(root); err != nil {
			return false, true, nil
		}
		markerPath := filepath.Join(root, runtimeMarkerName)
		marker, markerErr := os.ReadFile(markerPath)
		if markerErr != nil || string(marker) != runtimeMarkerContent() {
			if markerErr != nil && !os.IsNotExist(markerErr) {
				if markerInfo, statErr := os.Lstat(markerPath); statErr != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
					return false, false, markerErr
				}
			}
			if err := writeAtomicFile(markerPath, []byte(runtimeMarkerContent()), 0o600); err != nil {
				return false, false, err
			}
		}
		return true, true, nil
	}
	repairable, err = runtimeTreeOwned(root, layout)
	return false, repairable, err
}

func runtimeTreeOwned(root string, layout *runtimeLayout) (bool, error) {
	owned := true
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := strings.ToLower(filepath.Clean(relative))
		if info.Mode()&os.ModeSymlink != 0 {
			owned = false
			return filepath.SkipDir
		}
		if info.IsDir() {
			if _, expected := layout.directories[key]; !expected {
				owned = false
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			owned = false
			return nil
		}
		if key == strings.ToLower(runtimeMarkerName) || key == strings.ToLower(prepareMarkerName) || key == "my.ini" {
			return nil
		}
		if _, expected := layout.files[key]; !expected {
			owned = false
		}
		return nil
	})
	return owned, err
}

func runtimeMatchesPinnedArchive(ctx context.Context, root string, layout *runtimeLayout) (bool, error) {
	owned, err := runtimeTreeOwned(root, layout)
	if err != nil || !owned {
		return false, err
	}
	paths := make([]string, 0, len(layout.files))
	for relative := range layout.files {
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	buffer := make([]byte, 1024*1024)
	for _, relative := range paths {
		matches, err := runtimeFileMatchesArchive(ctx, filepath.Join(root, relative), layout.files[relative], buffer)
		if err != nil {
			return false, err
		}
		if !matches {
			return false, nil
		}
	}
	return true, nil
}

func runtimeFileMatchesArchive(ctx context.Context, path string, entry *zip.File, buffer []byte) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != int64(entry.UncompressedSize64) {
		return false, nil
	}
	archiveInput, err := entry.Open()
	if err != nil {
		return false, err
	}
	archiveHash := sha256.New()
	archiveBytes, archiveErr := copyWithContext(ctx, archiveHash, archiveInput, buffer)
	archiveCloseErr := archiveInput.Close()
	if archiveErr != nil {
		return false, archiveErr
	}
	if archiveCloseErr != nil {
		return false, archiveCloseErr
	}
	if archiveBytes != int64(entry.UncompressedSize64) {
		return false, fmt.Errorf("pinned MySQL archive entry has an unexpected size: %s", entry.Name)
	}
	installedInput, err := os.Open(path)
	if err != nil {
		return false, err
	}
	installedHash := sha256.New()
	installedBytes, installedErr := copyWithContext(ctx, installedHash, installedInput, buffer)
	installedCloseErr := installedInput.Close()
	if installedErr != nil {
		return false, installedErr
	}
	if installedCloseErr != nil {
		return false, installedCloseErr
	}
	return installedBytes == archiveBytes && bytes.Equal(installedHash.Sum(nil), archiveHash.Sum(nil)), nil
}

func validateRuntimeRequired(runtimeRoot string) error {
	for _, required := range []string{
		filepath.Join(runtimeRoot, "bin", "mysqld.exe"),
		filepath.Join(runtimeRoot, "bin", "mysql.exe"),
		filepath.Join(runtimeRoot, "data-template", "mysql", "user.frm"),
	} {
		if info, err := os.Lstat(required); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("MySQL runtime is missing %s", required)
		}
	}
	return nil
}

func runtimeMarkerContent() string {
	return "KiemTheDeployForge\r\nmysql=5.5.15-win32\r\nsha256=" + PinnedMySQLSHA256 + "\r\n"
}

func copyDataTemplate(source, destination string) error {
	return copyDataTemplateContext(context.Background(), source, destination)
}

func copyDataTemplateContext(ctx context.Context, source, destination string) error {
	ready, repairable, err := inspectDataDirectory(source, destination)
	if err != nil {
		return err
	}
	if ready {
		return removeOwnedPreparationMarker(destination, "mysql-data")
	}
	if !repairable {
		return fmt.Errorf("existing MySQL data is neither a valid database nor an installer-owned partial copy: %s", destination)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if err := cleanupOwnedPreparationDirectories(parent, dataPrepPrefix, "mysql-data"); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, dataPrepPrefix)
	if err != nil {
		return err
	}
	keepStaging := true
	defer func() {
		if keepStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := markPreparationDirectory(staging, "mysql-data"); err != nil {
		return err
	}
	if err := copyTemplateTreeContext(ctx, source, staging); err != nil {
		return err
	}
	copyReady, _, err := inspectDataDirectory(source, staging)
	if err != nil {
		return err
	}
	if !copyReady {
		return fmt.Errorf("staged MySQL data template did not verify")
	}
	if err := writeAtomicFile(filepath.Join(staging, dataMarkerName), []byte("KiemTheDeployForge\r\nstate=template-ready\r\n"), 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		if err := os.RemoveAll(destination); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	keepStaging = false
	return removeOwnedPreparationMarker(destination, "mysql-data")
}

type templateEntry struct {
	path string
	info os.FileInfo
}

func inspectDataDirectory(source, destination string) (ready, repairable bool, returnErr error) {
	expectedFiles := make(map[string]templateEntry)
	expectedDirectories := make(map[string]struct{})
	err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		key := strings.ToLower(filepath.Clean(relative))
		if info.IsDir() {
			expectedDirectories[key] = struct{}{}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported data template entry %s", path)
		}
		expectedFiles[key] = templateEntry{path: path, info: info}
		return nil
	})
	if err != nil {
		return false, false, err
	}
	info, err := os.Lstat(destination)
	if os.IsNotExist(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, false, nil
	}
	matched := make(map[string]struct{}, len(expectedFiles))
	hasExtra := false
	hasMismatch := false
	err = filepath.Walk(destination, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == destination {
			return nil
		}
		relative, err := filepath.Rel(destination, path)
		if err != nil {
			return err
		}
		key := strings.ToLower(filepath.Clean(relative))
		if info.Mode()&os.ModeSymlink != 0 {
			hasExtra = true
			return filepath.SkipDir
		}
		if info.IsDir() {
			if _, expected := expectedDirectories[key]; !expected {
				hasExtra = true
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			hasExtra = true
			return nil
		}
		if key == strings.ToLower(dataMarkerName) || key == strings.ToLower(prepareMarkerName) {
			return nil
		}
		expected, exists := expectedFiles[key]
		if !exists {
			hasExtra = true
			return nil
		}
		same, err := sameFile(expected.path, path, expected.info.Size(), info.Size())
		if err != nil {
			return err
		}
		if same {
			matched[key] = struct{}{}
		} else {
			hasMismatch = true
		}
		return nil
	})
	if err != nil {
		return false, false, err
	}
	if !hasExtra && !hasMismatch && len(matched) == len(expectedFiles) {
		return true, true, nil
	}
	if !hasExtra {
		return false, true, nil
	}
	required := filepath.Join(destination, "mysql", "user.frm")
	if requiredInfo, err := os.Lstat(required); err == nil && requiredInfo.Mode().IsRegular() && requiredInfo.Size() > 0 {
		// MySQL creates additional root files and schema directories after first
		// start. Preserve that initialized database even if template files changed.
		return true, false, nil
	}
	return false, false, nil
}

func copyTemplateTree(source, destination string) error {
	return copyTemplateTreeContext(context.Background(), source, destination)
}

func copyTemplateTreeContext(ctx context.Context, source, destination string) error {
	buffer := make([]byte, 1024*1024)
	return filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported data template entry %s", path)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			input.Close()
			return err
		}
		written, copyErr := copyWithContext(ctx, output, input, buffer)
		var syncErr error
		if copyErr == nil {
			syncErr = output.Sync()
		}
		closeErr := output.Close()
		inputCloseErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if written != info.Size() {
			return fmt.Errorf("short copy for %s", path)
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		if inputCloseErr != nil {
			return inputCloseErr
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader, buffer []byte) (int64, error) {
	var written int64
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			copied, writeErr := destination.Write(buffer[:read])
			written += int64(copied)
			if writeErr != nil {
				return written, writeErr
			}
			if copied != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func preparationMarkerContent(kind string) string {
	return "KiemTheDeployForge\r\nkind=" + kind + "\r\n"
}

func markPreparationDirectory(path, kind string) error {
	markerPath := filepath.Join(path, prepareMarkerName)
	return os.WriteFile(markerPath, []byte(preparationMarkerContent(kind)), 0o600)
}

func removeOwnedPreparationMarker(path, kind string) error {
	markerPath := filepath.Join(path, prepareMarkerName)
	info, err := os.Lstat(markerPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid MySQL preparation marker preserved: %s", markerPath)
	}
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return err
	}
	if string(raw) != preparationMarkerContent(kind) {
		return fmt.Errorf("unowned MySQL preparation marker preserved: %s", markerPath)
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func cleanupOwnedPreparationDirectories(parent, prefix, kind string) error {
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cleanupErrors []error
	lowerPrefix := strings.ToLower(prefix)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(strings.ToLower(name), lowerPrefix) || len(name) <= len(prefix) {
			continue
		}
		path := filepath.Join(parent, name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if !os.IsNotExist(statErr) {
				cleanupErrors = append(cleanupErrors, statErr)
			}
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		markerPath := filepath.Join(path, prepareMarkerName)
		markerInfo, markerErr := os.Lstat(markerPath)
		if markerErr != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		raw, readErr := os.ReadFile(markerPath)
		if readErr != nil || string(raw) != preparationMarkerContent(kind) {
			continue
		}
		if removeErr := os.RemoveAll(path); removeErr != nil && !os.IsNotExist(removeErr) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove abandoned MySQL preparation directory %s: %w", path, removeErr))
		}
	}
	return errors.Join(cleanupErrors...)
}

func sameFile(left, right string, leftSize, rightSize int64) (bool, error) {
	if leftSize != rightSize {
		return false, nil
	}
	leftFile, err := os.Open(left)
	if err != nil {
		return false, err
	}
	defer leftFile.Close()
	rightFile, err := os.Open(right)
	if err != nil {
		return false, err
	}
	defer rightFile.Close()
	leftHash := sha256.New()
	rightHash := sha256.New()
	buffer := make([]byte, 1024*1024)
	if _, err := io.CopyBuffer(leftHash, leftFile, buffer); err != nil {
		return false, err
	}
	if _, err := io.CopyBuffer(rightHash, rightFile, buffer); err != nil {
		return false, err
	}
	return bytes.Equal(leftHash.Sum(nil), rightHash.Sum(nil)), nil
}

func writeMyINI(path, runtimeRoot, dataRoot string) error {
	return writeAtomicFile(path, myINIContent(runtimeRoot, dataRoot), 0o600)
}

func verifyMyINI(path, runtimeRoot, dataRoot string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("managed MySQL configuration is not a regular file: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	expected := myINIContent(runtimeRoot, dataRoot)
	if !bytes.Equal(raw, expected) && !strings.EqualFold(string(raw), string(expected)) {
		return fmt.Errorf("managed MySQL configuration drifted from the installer-owned policy: %s", path)
	}
	return nil
}

func myINIContent(runtimeRoot, dataRoot string) []byte {
	normalize := func(value string) string { return strings.ReplaceAll(value, "\\", "/") }
	content := fmt.Sprintf("[client]\r\nport=3306\r\nprotocol=tcp\r\n\r\n[mysqld]\r\nbasedir=%s\r\ndatadir=%s\r\nport=3306\r\nbind-address=127.0.0.1\r\ncharacter-set-server=latin1\r\ndefault-storage-engine=INNODB\r\nlower_case_table_names=1\r\nsql-mode=STRICT_TRANS_TABLES,NO_AUTO_CREATE_USER,NO_ENGINE_SUBSTITUTION\r\nmax_connections=100\r\nmax_allowed_packet=64M\r\nskip-name-resolve\r\n", normalize(runtimeRoot), normalize(dataRoot))
	return []byte(content)
}

func writeAtomicFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keep := true
	defer func() {
		_ = temp.Close()
		if keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if _, err := temp.Write(content); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.Rename(tempPath, path); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err := winfile.Replace(tempPath, path); err != nil {
		return err
	}
	keep = false
	return nil
}

type databaseImportState struct {
	FormatVersion  int      `json:"formatVersion"`
	Status         string   `json:"status"`
	SQLSHA256      string   `json:"sqlSha256"`
	StageDatabase  string   `json:"stageDatabase,omitempty"`
	ImportUser     string   `json:"importUser,omitempty"`
	ExpectedTables []string `json:"expectedTables"`
	UpdatedUTC     string   `json:"updatedUtc"`
}

func ensureDatabase(ctx context.Context, mysql, sqlPath, sqlSHA256 string, validation SQLValidation, markerPath string, adoptComplete bool, checkImportSpace func() error) (bool, error) {
	expected := normalizedTables(validation.ExpectedTables)
	if len(expected) == 0 {
		return false, fmt.Errorf("jxaccount SQL has no expected tables")
	}
	if _, err := mysqlCommand(ctx, mysql, true, "CREATE DATABASE IF NOT EXISTS jxaccount DEFAULT CHARACTER SET latin1", nil); err != nil {
		return false, err
	}
	targetTables, err := databaseTables(ctx, mysql, "jxaccount")
	if err != nil {
		return false, err
	}
	state, err := loadDatabaseImportState(markerPath)
	if err != nil {
		return false, err
	}
	if state != nil {
		if !strings.EqualFold(state.SQLSHA256, sqlSHA256) || !sameTables(state.ExpectedTables, expected) {
			return false, fmt.Errorf("jxaccount import marker belongs to another SQL artifact")
		}
		switch state.Status {
		case "complete":
			if err := validateDatabaseSchema(ctx, mysql, "jxaccount", expected, false); err != nil {
				return false, fmt.Errorf("completed jxaccount schema drifted: %w", err)
			}
			return false, nil
		case "importing":
			if !validStageDatabase(state.StageDatabase) {
				return false, fmt.Errorf("jxaccount import marker contains an unsafe staging database")
			}
			if state.ImportUser != "" {
				if !validImportUser(state.ImportUser) {
					return false, fmt.Errorf("jxaccount import marker contains an unsafe temporary user")
				}
				if err := dropImportUser(mysql, state.ImportUser); err != nil {
					return false, err
				}
				state.ImportUser = ""
			}
			if len(targetTables) != 0 {
				if !sameTables(targetTables, expected) {
					return false, fmt.Errorf("jxaccount is non-empty while an import is incomplete; refusing destructive recovery")
				}
				if err := validateDatabaseSchema(ctx, mysql, "jxaccount", expected, true); err != nil {
					return false, err
				}
				if _, err := mysqlCommand(ctx, mysql, true, "DROP DATABASE IF EXISTS "+quoteIdentifier(state.StageDatabase), nil); err != nil {
					return false, err
				}
				state.Status = "complete"
				state.StageDatabase = ""
				if err := writeDatabaseImportState(markerPath, state); err != nil {
					return false, err
				}
				return true, nil
			}
			if _, err := mysqlCommand(ctx, mysql, true, "DROP DATABASE IF EXISTS "+quoteIdentifier(state.StageDatabase), nil); err != nil {
				return false, err
			}
		default:
			return false, fmt.Errorf("unknown jxaccount import marker status %q", state.Status)
		}
	} else {
		if len(targetTables) != 0 {
			if !adoptComplete {
				return false, fmt.Errorf("jxaccount already contains tables without an installer completion marker; preserving it unchanged")
			}
			if !sameTables(targetTables, expected) {
				return false, fmt.Errorf("legacy jxaccount schema does not match this SQL artifact")
			}
			if err := validateDatabaseSchema(ctx, mysql, "jxaccount", expected, true); err != nil {
				return false, err
			}
			state = &databaseImportState{FormatVersion: 1, Status: "complete", SQLSHA256: strings.ToLower(sqlSHA256), ExpectedTables: expected}
			if err := writeDatabaseImportState(markerPath, state); err != nil {
				return false, err
			}
			return false, nil
		}
		stageDatabase, err := newStageDatabase(ctx, mysql)
		if err != nil {
			return false, err
		}
		state = &databaseImportState{
			FormatVersion: 1, Status: "importing", SQLSHA256: strings.ToLower(sqlSHA256),
			StageDatabase: stageDatabase, ExpectedTables: expected,
		}
		if err := writeDatabaseImportState(markerPath, state); err != nil {
			return false, err
		}
	}

	if err := requireImportSpace(checkImportSpace); err != nil {
		return false, err
	}
	if _, err := mysqlCommand(ctx, mysql, true, "CREATE DATABASE "+quoteIdentifier(state.StageDatabase)+" DEFAULT CHARACTER SET latin1", nil); err != nil {
		return false, fmt.Errorf("create staging database: %w", err)
	}
	sql, err := os.ReadFile(sqlPath)
	if err != nil {
		return false, errors.Join(err, dropStageDatabase(mysql, state.StageDatabase))
	}
	importUser, importPassword, err := newUniqueImportCredential(ctx, mysql)
	if err != nil {
		return false, errors.Join(err, dropStageDatabase(mysql, state.StageDatabase))
	}
	state.ImportUser = importUser
	if err := writeDatabaseImportState(markerPath, state); err != nil {
		return false, errors.Join(err, dropStageDatabase(mysql, state.StageDatabase))
	}
	if err := createImportUser(ctx, mysql, importUser, importPassword, state.StageDatabase); err != nil {
		return false, errors.Join(fmt.Errorf("create restricted SQL importer: %w", err), dropImportUser(mysql, importUser), dropStageDatabase(mysql, state.StageDatabase))
	}
	_, importErr := mysqlCommandDatabaseAsGuarded(ctx, mysql, importUser, importPassword, state.StageDatabase, "", sql, checkImportSpace)
	cleanupErr := dropImportUser(mysql, importUser)
	var markerErr error
	if cleanupErr == nil {
		state.ImportUser = ""
		markerErr = writeDatabaseImportState(markerPath, state)
	}
	var stageCleanupErr error
	if importErr != nil {
		stageCleanupErr = dropStageDatabase(mysql, state.StageDatabase)
	}
	if importErr != nil || cleanupErr != nil || markerErr != nil || stageCleanupErr != nil {
		var wrappedImportErr error
		if importErr != nil {
			wrappedImportErr = fmt.Errorf("import jxaccount SQL into staging database: %w", importErr)
		}
		return false, errors.Join(wrappedImportErr, cleanupErr, markerErr, stageCleanupErr)
	}
	if err := validateDatabaseSchema(ctx, mysql, state.StageDatabase, expected, true); err != nil {
		return false, errors.Join(fmt.Errorf("validate staged jxaccount database: %w", err), dropStageDatabase(mysql, state.StageDatabase))
	}
	targetTables, err = databaseTables(ctx, mysql, "jxaccount")
	if err != nil {
		return false, errors.Join(err, dropStageDatabase(mysql, state.StageDatabase))
	}
	if len(targetTables) != 0 {
		return false, errors.Join(fmt.Errorf("jxaccount changed while staging the import; preserving it unchanged"), dropStageDatabase(mysql, state.StageDatabase))
	}
	renames := make([]string, 0, len(expected))
	for _, table := range expected {
		renames = append(renames, quoteIdentifier(state.StageDatabase)+"."+quoteIdentifier(table)+" TO `jxaccount`."+quoteIdentifier(table))
	}
	if _, err := mysqlCommand(ctx, mysql, true, "RENAME TABLE "+strings.Join(renames, ", "), nil); err != nil {
		return false, errors.Join(fmt.Errorf("publish staged jxaccount database: %w", err), dropStageDatabase(mysql, state.StageDatabase))
	}
	if err := validateDatabaseSchema(ctx, mysql, "jxaccount", expected, true); err != nil {
		return false, fmt.Errorf("validate published jxaccount database: %w", err)
	}
	if _, err := mysqlCommand(ctx, mysql, true, "DROP DATABASE IF EXISTS "+quoteIdentifier(state.StageDatabase), nil); err != nil {
		return false, err
	}
	state.Status = "complete"
	state.StageDatabase = ""
	if err := writeDatabaseImportState(markerPath, state); err != nil {
		return false, err
	}
	return true, nil
}

func requireImportSpace(check func() error) error {
	if check == nil {
		return nil
	}
	if err := check(); err != nil {
		return fmt.Errorf("database import disk reserve is unavailable: %w", err)
	}
	return nil
}

func validateDatabaseSchema(ctx context.Context, mysql, database string, expected []string, exact bool) error {
	tables, err := databaseTables(ctx, mysql, database)
	if err != nil {
		return err
	}
	if (exact && !sameTables(tables, expected)) || (!exact && !containsTables(tables, expected)) {
		return fmt.Errorf("database %s tables=%v want=%v", database, tables, expected)
	}
	columnCount, err := mysqlScalar(ctx, mysql, true, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='"+sqlLiteral(database)+"' AND table_name='account' AND column_name IN ('loginName','password_hash')")
	if err != nil || strings.TrimSpace(columnCount) != "2" {
		return fmt.Errorf("%s.account is missing loginName/password_hash", database)
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	probeTable := "ktf_probe_" + hex.EncodeToString(random)
	probe := schemaProbeQuery(database, probeTable)
	if _, err := mysqlCommandDatabase(ctx, mysql, true, database, probe, nil); err != nil {
		return fmt.Errorf("%s.account insert compatibility check failed: %w", database, err)
	}
	return nil
}

func schemaProbeQuery(database, probeTable string) string {
	return "CREATE TEMPORARY TABLE " + quoteIdentifier(probeTable) + " LIKE " + quoteIdentifier(database) + ".`account`;" +
		"INSERT INTO " + quoteIdentifier(probeTable) + "(loginName,password_hash) VALUES('__kt_installer_probe__','00000000000000000000000000000000');" +
		"DROP TEMPORARY TABLE " + quoteIdentifier(probeTable) + ";"
}

func databaseTables(ctx context.Context, mysql, database string) ([]string, error) {
	output, err := mysqlScalar(ctx, mysql, true, "SELECT table_name FROM information_schema.tables WHERE table_schema='"+sqlLiteral(database)+"' ORDER BY table_name")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(output) == "" {
		return nil, nil
	}
	return normalizedTables(strings.Fields(output)), nil
}

func normalizedTables(tables []string) []string {
	seen := make(map[string]struct{}, len(tables))
	result := make([]string, 0, len(tables))
	for _, table := range tables {
		table = strings.ToLower(strings.TrimSpace(table))
		if table == "" {
			continue
		}
		if _, exists := seen[table]; exists {
			continue
		}
		seen[table] = struct{}{}
		result = append(result, table)
	}
	sort.Strings(result)
	return result
}

func sameTables(left, right []string) bool {
	left = normalizedTables(left)
	right = normalizedTables(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func containsTables(actual, expected []string) bool {
	available := make(map[string]struct{}, len(actual))
	for _, table := range normalizedTables(actual) {
		available[table] = struct{}{}
	}
	for _, table := range normalizedTables(expected) {
		if _, exists := available[table]; !exists {
			return false
		}
	}
	return true
}

func newStageDatabase(ctx context.Context, mysql string) (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 8)
		if _, err := rand.Read(random); err != nil {
			return "", err
		}
		name := stagePrefix + hex.EncodeToString(random)
		count, err := mysqlScalar(ctx, mysql, true, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='"+name+"'")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(count) == "0" {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not reserve a unique MySQL staging database")
}

func loadDatabaseImportState(path string) (*databaseImportState, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state databaseImportState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("read jxaccount import marker: %w", err)
	}
	if state.FormatVersion != 1 || (state.Status != "importing" && state.Status != "complete") || len(state.ExpectedTables) == 0 {
		return nil, fmt.Errorf("jxaccount import marker is invalid")
	}
	if state.ImportUser != "" && !validImportUser(state.ImportUser) {
		return nil, fmt.Errorf("jxaccount import marker has an invalid restricted importer")
	}
	if state.Status == "complete" && (state.StageDatabase != "" || state.ImportUser != "") {
		return nil, fmt.Errorf("completed jxaccount import marker still owns temporary resources")
	}
	return &state, nil
}

func writeDatabaseImportState(path string, state *databaseImportState) error {
	state.UpdatedUTC = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeAtomicFile(path, raw, 0o600)
}

func validStageDatabase(name string) bool {
	if !strings.HasPrefix(name, stagePrefix) || len(name) != len(stagePrefix)+16 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(name, stagePrefix))
	return err == nil
}

func validImportUser(name string) bool {
	if !strings.HasPrefix(name, "ktf") || len(name) != 15 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(name, "ktf"))
	return err == nil
}

func newImportCredential() (string, string, error) {
	userRandom := make([]byte, 6)
	passwordRandom := make([]byte, 24)
	if _, err := rand.Read(userRandom); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(passwordRandom); err != nil {
		return "", "", err
	}
	return "ktf" + hex.EncodeToString(userRandom), hex.EncodeToString(passwordRandom), nil
}

func newUniqueImportCredential(ctx context.Context, mysql string) (string, string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		user, password, err := newImportCredential()
		if err != nil {
			return "", "", err
		}
		count, err := mysqlScalar(ctx, mysql, true, "SELECT COUNT(*) FROM mysql.user WHERE User='"+sqlLiteral(user)+"' AND Host='127.0.0.1'")
		if err != nil {
			return "", "", err
		}
		if strings.TrimSpace(count) == "0" {
			return user, password, nil
		}
	}
	return "", "", fmt.Errorf("could not reserve a unique restricted SQL importer")
}

func createImportUser(ctx context.Context, mysql, user, password, database string) error {
	if !validImportUser(user) || !validStageDatabase(database) {
		return fmt.Errorf("unsafe restricted importer identity")
	}
	account := "'" + sqlLiteral(user) + "'@'127.0.0.1'"
	if _, err := mysqlCommand(ctx, mysql, true, "CREATE USER "+account+" IDENTIFIED BY '"+sqlLiteral(password)+"'", nil); err != nil {
		return err
	}
	if _, err := mysqlCommand(ctx, mysql, true, "GRANT ALL PRIVILEGES ON "+quoteIdentifier(database)+".* TO "+account, nil); err != nil {
		return errors.Join(err, dropImportUser(mysql, user))
	}
	return nil
}

func dropImportUser(mysql, user string) error {
	if !validImportUser(user) {
		return fmt.Errorf("unsafe restricted importer identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	account := "'" + sqlLiteral(user) + "'@'127.0.0.1'"
	count, err := mysqlScalar(ctx, mysql, true, "SELECT COUNT(*) FROM mysql.user WHERE User='"+sqlLiteral(user)+"' AND Host='127.0.0.1'")
	if err != nil {
		return fmt.Errorf("inspect restricted SQL importer %s: %w", user, err)
	}
	if strings.TrimSpace(count) == "0" {
		return nil
	}
	_, err = mysqlCommand(ctx, mysql, true, "DROP USER "+account, nil)
	if err != nil {
		return fmt.Errorf("remove restricted SQL importer %s: %w", user, err)
	}
	return nil
}

func dropStageDatabase(mysql, database string) error {
	if !validStageDatabase(database) {
		return fmt.Errorf("unsafe staging database identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := mysqlCommand(ctx, mysql, true, "DROP DATABASE IF EXISTS "+quoteIdentifier(database), nil); err != nil {
		return fmt.Errorf("remove incomplete staging database %s: %w", database, err)
	}
	return nil
}

func quoteIdentifier(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}

func sqlLiteral(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func mysqlScalar(ctx context.Context, mysql string, passwordSet bool, query string) (string, error) {
	output, err := mysqlCommand(ctx, mysql, passwordSet, query, nil)
	return strings.TrimSpace(output), err
}

func mysqlCommand(ctx context.Context, mysql string, passwordSet bool, query string, stdin []byte) (string, error) {
	args := []string{"--protocol=tcp", "--host=127.0.0.1", "--port=3306", "--user=root", "--batch", "--skip-column-names"}
	if query != "" {
		args = append(args, "--execute", query)
	} else {
		args = append(args, "jxaccount")
	}
	return runMySQLCommand(ctx, mysql, passwordSet, args, stdin)
}

func mysqlCommandDatabase(ctx context.Context, mysql string, passwordSet bool, database, query string, stdin []byte) (string, error) {
	args := []string{"--protocol=tcp", "--host=127.0.0.1", "--port=3306", "--user=root", "--batch", "--skip-column-names", "--database=" + database}
	if query != "" {
		args = append(args, "--execute", query)
	}
	return runMySQLCommand(ctx, mysql, passwordSet, args, stdin)
}

func mysqlCommandDatabaseAs(ctx context.Context, mysql, user, password, database, query string, stdin []byte) (string, error) {
	args := []string{"--protocol=tcp", "--host=127.0.0.1", "--port=3306", "--user=" + user, "--batch", "--skip-column-names", "--database=" + database}
	if query != "" {
		args = append(args, "--execute", query)
	}
	return runMySQLCommandWithPassword(ctx, mysql, password, args, stdin)
}

func mysqlCommandDatabaseAsGuarded(ctx context.Context, mysql, user, password, database, query string, stdin []byte, checkSpace func() error) (string, error) {
	if checkSpace == nil {
		return mysqlCommandDatabaseAs(ctx, mysql, user, password, database, query, stdin)
	}
	if err := requireImportSpace(checkSpace); err != nil {
		return "", err
	}
	guardedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	done := make(chan struct{})
	guardErrors := make(chan error, 1)
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-guardedCtx.Done():
				return
			case <-ticker.C:
				if err := checkSpace(); err != nil {
					guardErrors <- err
					cancel()
					return
				}
			}
		}
	}()
	output, commandErr := mysqlCommandDatabaseAs(guardedCtx, mysql, user, password, database, query, stdin)
	close(stop)
	<-done
	select {
	case guardErr := <-guardErrors:
		return output, errors.Join(fmt.Errorf("database import stopped to preserve free disk space: %w", guardErr), commandErr)
	default:
		return output, commandErr
	}
}

func runMySQLCommand(ctx context.Context, mysql string, passwordSet bool, args []string, stdin []byte) (string, error) {
	password := ""
	if passwordSet {
		password = activeAccounts.RootPassword
	}
	return runMySQLCommandWithPassword(ctx, mysql, password, args, stdin)
}

func runMySQLCommandWithPassword(ctx context.Context, mysql, password string, args []string, stdin []byte) (string, error) {
	command := exec.CommandContext(ctx, mysql, args...)
	winprocess.Hide(command)
	command.Env = mysqlEnvironmentForPassword(password)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("mysql command failed: %w (%s)", err, cleanOutput(string(output)))
	}
	return string(output), nil
}

func mysqlEnvironment(passwordSet bool) []string {
	password := ""
	if passwordSet {
		password = activeAccounts.RootPassword
	}
	return mysqlEnvironmentForPassword(password)
}

func mysqlEnvironmentForPassword(password string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if strings.EqualFold(name, "MYSQL_PWD") {
			continue
		}
		environment = append(environment, item)
	}
	if password != "" {
		environment = append(environment, "MYSQL_PWD="+password)
	}
	return environment
}

func waitForMySQL(ctx context.Context, mysql string, passwordSet bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := mysqlProbe(ctx, mysql, passwordSet); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("MySQL did not become ready: %w", lastErr)
}

func waitForEitherCredential(ctx context.Context, mysql string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := mysqlProbe(ctx, mysql, true); err == nil {
			return true, nil
		} else {
			lastErr = err
		}
		if _, err := mysqlProbe(ctx, mysql, false); err == nil {
			return false, nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return false, fmt.Errorf("MySQL did not become ready: %w", lastErr)
}

func mysqlProbe(ctx context.Context, mysql string, passwordSet bool) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return mysqlScalar(probeCtx, mysql, passwordSet, "SELECT 1")
}

func secureInitialAccounts(ctx context.Context, mysql string, passwordSet bool) error {
	rootLiteral := sqlLiteral(activeAccounts.RootPassword)
	bootstrapSQL := "UPDATE mysql.user SET Password=PASSWORD('" + rootLiteral + "') WHERE User='" + sqlLiteral(RootUser) + "';" +
		"DELETE FROM mysql.user WHERE User='';" +
		"DROP DATABASE IF EXISTS test;" +
		"DELETE FROM mysql.db WHERE Db='test' OR Db LIKE 'test\\_%';" +
		"FLUSH PRIVILEGES;"
	if _, err := mysqlCommand(ctx, mysql, passwordSet, bootstrapSQL, nil); err != nil {
		return fmt.Errorf("secure initial MySQL accounts: %w", err)
	}
	unsafeAccounts, err := mysqlScalar(ctx, mysql, true, "SELECT COUNT(*) FROM mysql.user WHERE User='' OR (User='"+sqlLiteral(RootUser)+"' AND Password<>PASSWORD('"+rootLiteral+"'))")
	if err != nil {
		return fmt.Errorf("verify secured MySQL accounts: %w", err)
	}
	if strings.TrimSpace(unsafeAccounts) != "0" {
		return fmt.Errorf("MySQL account hardening did not complete")
	}
	return nil
}

type windowsServiceInfo struct {
	Exists     bool
	Running    bool
	BinaryPath string
}

func queryWindowsService(name string) (windowsServiceInfo, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return windowsServiceInfo{}, fmt.Errorf("connect to Windows service manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return windowsServiceInfo{}, nil
		}
		return windowsServiceInfo{}, fmt.Errorf("open service %s: %w", name, err)
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return windowsServiceInfo{}, fmt.Errorf("query service %s status: %w", name, err)
	}
	config, err := service.Config()
	if err != nil {
		return windowsServiceInfo{}, fmt.Errorf("query service %s configuration: %w", name, err)
	}
	return windowsServiceInfo{Exists: true, Running: status.State == svc.Running, BinaryPath: config.BinaryPathName}, nil
}

func serviceCommandOwned(commandLine, mysqld, iniPath string) (bool, error) {
	arguments, err := windows.DecomposeCommandLine(commandLine)
	if err != nil {
		return false, fmt.Errorf("parse managed service command line: %w", err)
	}
	if len(arguments) == 0 || !sameWindowsPath(arguments[0], mysqld) {
		return false, nil
	}
	for _, argument := range arguments[1:] {
		const prefix = "--defaults-file="
		if strings.HasPrefix(strings.ToLower(argument), prefix) && sameWindowsPath(argument[len(prefix):], iniPath) {
			return true, nil
		}
	}
	return false, nil
}

func sameWindowsPath(left, right string) bool {
	left = strings.Trim(strings.TrimSpace(left), `"`)
	right = strings.Trim(strings.TrimSpace(right), `"`)
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(leftAbsolute), filepath.Clean(rightAbsolute))
}

func portOpen(address string) bool {
	connection, err := net.DialTimeout("tcp4", address, 800*time.Millisecond)
	if err != nil {
		return false
	}
	connection.Close()
	return true
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	winprocess.Hide(command)
	output, err := command.CombinedOutput()
	return string(output), err
}

func record(options MySQLOptions, phase, resource, action, status string, owned bool, operationErr error) {
	if options.Record != nil {
		options.Record(phase, resource, action, status, owned, operationErr)
	}
}

func status(err error) string {
	if err != nil {
		return "failed"
	}
	return "completed"
}

// redactSecrets keeps configured credentials out of logs and error text. Both
// passwords are covered, not just root's, because either can appear in mysql
// client diagnostics.
func redactSecrets(output string) string {
	for _, secret := range []string{activeAccounts.RootPassword, activeAccounts.BotPassword} {
		if secret != "" {
			output = strings.ReplaceAll(output, secret, "[redacted]")
		}
	}
	return output
}

func cleanOutput(output string) string {
	output = redactSecrets(output)
	output = strings.TrimSpace(output)
	if len(output) > 4096 {
		output = output[len(output)-4096:]
	}
	return output
}
