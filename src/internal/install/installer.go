package install

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kiemthedeployforge/internal/configpatch"
	"kiemthedeployforge/internal/database"
	"kiemthedeployforge/internal/network"
	"kiemthedeployforge/internal/release"
	"kiemthedeployforge/internal/sfx"
	"kiemthedeployforge/internal/winfile"
)

type Options struct {
	SetupPath   string
	InstallRoot string
	// LANAddress overrides automatic detection. Leave it empty to detect.
	//
	// Detection reads the addresses actually bound to the machine's adapters,
	// which is everything a LAN server or a directly addressed hosted server
	// needs. It cannot work behind provider NAT, where the address clients must
	// use is held by the provider and never appears on the machine, so the
	// operator has to be able to supply it.
	LANAddress    string
	VerifyPackage bool
	Logf          func(string, ...any)
}

type Plan struct {
	ReleaseID      string            `json:"releaseId"`
	ManifestSHA256 string            `json:"manifestSha256"`
	SetupPath      string            `json:"setupPath"`
	MediaPath      string            `json:"mediaPath"`
	InstallRoot    string            `json:"installRoot"`
	PayloadFiles   int               `json:"payloadFiles"`
	PayloadBytes   int64             `json:"payloadBytes"`
	RequiredBytes  int64             `json:"requiredBytes"`
	AvailableBytes uint64            `json:"availableBytes"`
	TargetVolume   VolumeInfo        `json:"targetVolume"`
	LAN            network.Candidate `json:"lan"`
	// LANOverridden marks an address supplied by the operator rather than
	// detected.
	LANOverridden bool `json:"lanOverridden"`
	// LANError explains why detection found nothing. It is not fatal on its
	// own: the operator can still supply an address and continue.
	LANError        string           `json:"lanError,omitempty"`
	MySQLVersion    string           `json:"mysqlVersion"`
	PatchedKeys     int              `json:"patchedKeys"`
	PatchedBotKeys  int              `json:"patchedBotKeys"`
	IncludesBot     bool             `json:"includesBot"`
	Accounts        release.Accounts `json:"accounts"`
	PackageVerified bool             `json:"packageVerified"`

	// SystemDrive is the Windows drive the bot writes its working data to.
	// It is reported separately because the operator may install the release
	// on another volume while the bot still needs room on C:.
	SystemDrive          string `json:"systemDrive"`
	SystemDriveFreeBytes uint64 `json:"systemDriveFreeBytes"`
	BotRequiredBytes     uint64 `json:"botRequiredBytes"`
	// BotDiskWarning is empty when the system drive has enough room for the
	// bot. It never blocks the install; the operator is only warned.
	BotDiskWarning string `json:"botDiskWarning,omitempty"`
}

type State struct {
	Product        string               `json:"product"`
	ReleaseID      string               `json:"releaseId"`
	Status         string               `json:"status"`
	LastError      string               `json:"lastError,omitempty"`
	RecoveryAction string               `json:"recoveryAction,omitempty"`
	InstalledUTC   string               `json:"installedUtc"`
	InstallRoot    string               `json:"installRoot"`
	LANIP          string               `json:"lanIp"`
	ManifestSHA256 string               `json:"manifestSha256"`
	OperationID    string               `json:"operationId"`
	PatchedKeys    int                  `json:"patchedKeys"`
	PatchedBotKeys int                  `json:"patchedBotKeys"`
	RepairedFiles  int                  `json:"repairedFiles,omitempty"`
	Shortcuts      int                  `json:"shortcuts"`
	BotDiskWarning string               `json:"botDiskWarning,omitempty"`
	MySQL          database.MySQLResult `json:"mysql"`
	JournalPath    string               `json:"journalPath"`
}

type recordFunc func(phase, resource, action, status string, owned bool, operationErr error)

type stagingOwner struct {
	Product     string `json:"product"`
	ReleaseID   string `json:"releaseId"`
	InstallRoot string `json:"installRoot"`
}

const (
	installDiskHeadroom             = int64(2 * 1024 * 1024 * 1024)
	plannedMySQLExpansionAllowance  = int64(1024 * 1024 * 1024)
	plannedSQLExpansionMultiplier   = int64(4)
	minimumPostInstallFreeDiskBytes = uint64(2 * 1024 * 1024 * 1024)
)

func BuildPlan(ctx context.Context, options Options) (plan *Plan, err error) {
	installRoot, err := validateInstallRoot(options.InstallRoot)
	if err != nil {
		return nil, err
	}
	locks, err := acquireInstallLocks(installRoot)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, locks.Close()) }()
	options.InstallRoot = installRoot
	plan, media, err := openPlan(ctx, options)
	if media != nil {
		err = joinMediaCloseError(err, media.Close())
	}
	return plan, err
}

func joinMediaCloseError(operationErr, closeErr error) error {
	if closeErr == nil {
		return operationErr
	}
	return errors.Join(operationErr, fmt.Errorf("release offline installation media: %w", closeErr))
}

func openPlan(ctx context.Context, options Options) (*Plan, *installMedia, error) {
	setupPath, err := filepath.Abs(options.SetupPath)
	if err != nil {
		return nil, nil, err
	}
	installRoot, err := validateInstallRoot(options.InstallRoot)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateInstallTarget(installRoot, setupPath); err != nil {
		return nil, nil, err
	}
	media, err := openInstallMedia(ctx, setupPath)
	if err != nil {
		return nil, media, err
	}
	// A blank override means detect. A detection failure is recorded rather
	// than returned, so the preflight still reports the package and the disk
	// numbers and the operator can supply an address instead of being stuck.
	var lan network.Candidate
	var lanError string
	overridden := strings.TrimSpace(options.LANAddress) != ""
	if overridden {
		lan, err = network.Manual(options.LANAddress)
		if err != nil {
			return nil, media, err
		}
	} else if detected, detectErr := network.DetectContext(ctx); detectErr != nil {
		lanError = detectErr.Error()
	} else {
		lan = detected
	}
	available, err := AvailableBytes(filepath.Dir(installRoot))
	if err != nil {
		return nil, media, err
	}
	volume, err := RequireFixedNTFS(installRoot, "installation directory")
	if err != nil {
		return nil, media, err
	}
	required, err := requiredInstallBytes(media.payload.Manifest.PayloadBytes, media.payload.Manifest.Database.Size)
	if err != nil {
		return nil, media, err
	}
	verified := false
	if options.VerifyPackage {
		if err := media.payload.VerifyAll(ctx, nil); err != nil {
			return nil, media, err
		}
		verified = true
	}
	systemDrive, systemFree, err := SystemDriveFree()
	if err != nil {
		return nil, media, err
	}
	plan := &Plan{
		ReleaseID: media.payload.Manifest.ReleaseID, ManifestSHA256: media.payload.ManifestSHA256,
		SetupPath: setupPath, MediaPath: media.sourcePath(), InstallRoot: installRoot, PayloadFiles: len(media.payload.Manifest.Files),
		PayloadBytes: media.payload.Manifest.PayloadBytes, RequiredBytes: required, AvailableBytes: available,
		TargetVolume: volume,
		LAN:          lan, LANOverridden: overridden, LANError: lanError,
		MySQLVersion:    media.payload.Manifest.MySQL.Version,
		PatchedKeys:     len(configpatch.LANRules(lan.Address)),
		PatchedBotKeys:  len(botEnvRules(media.payload.Manifest, installRoot)),
		IncludesBot:     media.payload.Manifest.IncludesBot,
		Accounts:        media.payload.Manifest.Accounts.WithDefaults(),
		PackageVerified: verified,
		SystemDrive:     systemDrive, SystemDriveFreeBytes: systemFree, BotRequiredBytes: BotSystemDriveFreeBytes,
	}
	// The 20 GiB reserve is a bot requirement, so a release built without one
	// must not warn about it.
	if media.payload.Manifest.IncludesBot && systemFree < BotSystemDriveFreeBytes {
		plan.BotDiskWarning = fmt.Sprintf(
			"The bot needs at least %.0f GiB free on %s but only %.2f GiB is available. Installation can continue, but free up space on %s before running the bot.",
			float64(BotSystemDriveFreeBytes)/(1024*1024*1024), systemDrive,
			float64(systemFree)/(1024*1024*1024), systemDrive)
	}
	return plan, media, nil
}

func Run(ctx context.Context, options Options, progress func(percent int, stage, detail string)) (state *State, err error) {
	installRoot, err := validateInstallRoot(options.InstallRoot)
	if err != nil {
		return nil, err
	}
	locks, err := acquireInstallLocks(installRoot)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, locks.Close()) }()
	options.InstallRoot = installRoot
	plan, media, err := openPlan(ctx, options)
	if media != nil {
		defer func() {
			err = joinMediaCloseError(err, media.Close())
		}()
	}
	if err != nil {
		return nil, err
	}
	// Planning tolerates a missing address so the window can still show the
	// package and offer the field. Installing cannot: every configuration key
	// depends on it.
	if strings.TrimSpace(plan.LAN.Address) == "" {
		return nil, fmt.Errorf("no IPv4 address to write into the configuration; enter one manually (%s)", plan.LANError)
	}
	rootWasEmpty := false
	emptyRootRemoved := false
	defer func() {
		if !emptyRootRemoved {
			return
		}
		if mkdirErr := os.MkdirAll(plan.InstallRoot, 0o700); mkdirErr != nil {
			err = errors.Join(err, fmt.Errorf("restore selected empty install directory: %w", mkdirErr))
		}
	}()
	if info, statErr := os.Stat(plan.InstallRoot); statErr == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("install root exists and is not a directory: %s", plan.InstallRoot)
		}
		entries, readErr := os.ReadDir(plan.InstallRoot)
		if readErr != nil {
			return nil, readErr
		}
		if len(entries) == 0 {
			rootWasEmpty = true
		} else {
			return resumeCommittedInstall(ctx, options, plan, media.payload, progress)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	if err := cleanupStaleStages(plan.InstallRoot); err != nil {
		return nil, err
	}
	if plan.AvailableBytes < uint64(plan.RequiredBytes) {
		return nil, fmt.Errorf("insufficient target disk space: available=%d required=%d", plan.AvailableBytes, plan.RequiredBytes)
	}
	operationID, err := newOperationID()
	if err != nil {
		return nil, err
	}
	journal, err := NewJournal(operationID)
	if err != nil {
		return nil, err
	}
	defer journal.Close()
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	record := func(phase, resource, action, status string, owned bool, operationErr error) {
		_ = journal.Record(phase, resource, action, status, owned, operationErr)
	}
	if rootWasEmpty {
		if err := os.Remove(plan.InstallRoot); err != nil {
			return nil, fmt.Errorf("remove selected empty install directory: %w", err)
		}
		emptyRootRemoved = true
		record("install-root", plan.InstallRoot, "remove-empty-directory", "completed", true, nil)
	}
	stage := plan.InstallRoot + ".staging-" + operationID
	if err := os.MkdirAll(filepath.Dir(stage), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		return nil, fmt.Errorf("create exclusive staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed && strings.HasPrefix(stage, plan.InstallRoot+".staging-") {
			cleanupErr := removeStagingWithRetry(stage)
			err = errors.Join(err, cleanupErr)
		}
	}()
	stageOwnerPath := filepath.Join(stage, ".kiemthedeployforge-staging.json")
	if err := writeJSONAtomic(stageOwnerPath, stagingOwner{Product: "KiemTheDeployForge", ReleaseID: plan.ReleaseID, InstallRoot: plan.InstallRoot}); err != nil {
		return nil, err
	}
	record("staging-create", stage, "create", "completed", true, nil)
	if progress != nil {
		progress(5, "Extract payload", "Starting verified extraction")
	}
	err = media.payload.Extract(ctx, stage, func(item sfx.Progress) {
		percent := 5 + int(float64(item.CopiedBytes)/float64(max64(item.TotalBytes, 1))*55)
		if progress != nil {
			progress(percent, "Extract payload", item.Path)
		}
	})
	record("payload-extract", stage, "extract-and-verify", operationStatus(err), true, err)
	if err != nil {
		return nil, err
	}
	rules := configpatch.LANRules(plan.LAN.Address)
	botRules := botEnvRules(media.payload.Manifest, plan.InstallRoot)
	if progress != nil {
		progress(62, "Patch configuration", fmt.Sprintf("Applying detected LAN IPv4 to %d exact keys", len(rules)))
	}
	backupRoot := filepath.Join(stage, "InstallerData", "operations", operationID, "config-before")
	if err := configpatch.Apply(stage, rules, backupRoot); err != nil {
		record("config-patch", stage, "patch-lan-ip", "failed", true, err)
		return nil, err
	}
	if err := configpatch.Verify(stage, rules); err != nil {
		return nil, err
	}
	record("config-patch", stage, "patch-lan-ip", "completed", true, nil)
	// The bot resolves the server through an absolute path, so it is bound to
	// the committed install root rather than to the staging directory. A
	// release built without a bot has no rules and skips the step entirely.
	if len(botRules) > 0 {
		if progress != nil {
			progress(65, "Patch configuration", "Binding the bot to the installed server and MySQL account")
		}
		if err := configpatch.ApplyEnv(stage, botRules, backupRoot); err != nil {
			record("config-patch-bot", stage, "patch-bot-env", "failed", true, err)
			return nil, err
		}
		if err := configpatch.VerifyEnv(stage, botRules); err != nil {
			return nil, err
		}
		record("config-patch-bot", stage, "patch-bot-env", "completed", true, nil)
	}
	state = &State{
		Product: "KiemTheDeployForge", ReleaseID: media.payload.Manifest.ReleaseID, Status: "runtime-committed",
		InstalledUTC: time.Now().UTC().Format(time.RFC3339Nano), InstallRoot: plan.InstallRoot,
		LANIP: plan.LAN.Address, ManifestSHA256: plan.ManifestSHA256, OperationID: operationID,
		PatchedKeys: len(rules), PatchedBotKeys: len(botRules), JournalPath: journal.Path,
	}
	statePath := filepath.Join(stage, "InstallerData", "install-state.json")
	if err := writeJSONAtomic(statePath, state); err != nil {
		return nil, err
	}
	if err := os.Rename(stage, plan.InstallRoot); err != nil {
		record("install-commit", plan.InstallRoot, "rename-staging", "failed", true, err)
		return nil, err
	}
	committed = true
	emptyRootRemoved = false
	record("install-commit", plan.InstallRoot, "rename-staging", "completed", true, nil)
	if err := removeCommittedStagingMarker(plan.InstallRoot, plan.ReleaseID); err != nil {
		return state, fmt.Errorf("installation payload committed but its staging marker could not be removed; rerun the same Setup and directory: %w", err)
	}
	return finishCommittedInstall(ctx, options, plan, media.payload.Manifest, state, false, record, logf, progress)
}

func resumeCommittedInstall(ctx context.Context, options Options, plan *Plan, payload *sfx.Package, progress func(percent int, stage, detail string)) (*State, error) {
	manifest := payload.Manifest
	statePath := filepath.Join(plan.InstallRoot, "InstallerData", "install-state.json")
	state, err := loadState(statePath)
	if os.IsNotExist(err) {
		// The directory holds files this installer never wrote, so it is not a
		// half finished install to resume. Say that plainly instead of leaking
		// the missing state file path, which reads like a bug.
		return nil, fmt.Errorf(
			"%s already contains files but is not a previous installation of this release; "+
				"choose an empty or new directory, for example %s",
			plan.InstallRoot, filepath.Join(plan.InstallRoot, "KiemTheServer"))
	}
	if err != nil {
		return nil, fmt.Errorf("existing install cannot be resumed: %w", err)
	}
	if state.Product != "KiemTheDeployForge" || state.ReleaseID != manifest.ReleaseID || !strings.EqualFold(state.ManifestSHA256, plan.ManifestSHA256) || !strings.EqualFold(state.InstallRoot, plan.InstallRoot) {
		return nil, fmt.Errorf("existing install belongs to another release")
	}
	if err := removeCommittedStagingMarker(plan.InstallRoot, plan.ReleaseID); err != nil {
		return state, fmt.Errorf("remove committed staging marker: %w", err)
	}
	if progress != nil {
		progress(5, "Repair payload", "Verifying installed Client, Server and Bot files")
	}
	repaired, err := payload.Repair(ctx, plan.InstallRoot, payloadRepairMode, func(item sfx.Progress) {
		if progress != nil {
			percent := 5 + int(float64(item.CopiedBytes)/float64(max64(item.TotalBytes, 1))*50)
			progress(percent, "Repair payload", item.Path)
		}
	})
	if err != nil {
		return state, fmt.Errorf("repair installed payload: %w", err)
	}
	state.RepairedFiles = repaired
	if progress != nil {
		progress(58, "Repair payload", fmt.Sprintf("Verified payload; repaired %d file(s)", repaired))
	}
	wasComplete := state.Status == "complete"
	operationID, err := newOperationID()
	if err != nil {
		return state, err
	}
	journal, err := NewJournal(operationID)
	if err != nil {
		return state, err
	}
	defer journal.Close()
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	record := func(phase, resource, action, status string, owned bool, operationErr error) {
		_ = journal.Record(phase, resource, action, status, owned, operationErr)
	}
	rules := configpatch.LANRules(plan.LAN.Address)
	backupRoot := filepath.Join(plan.InstallRoot, "InstallerData", "operations", operationID, "config-before")
	configNeedsRepair := state.LANIP != plan.LAN.Address
	if !configNeedsRepair {
		configNeedsRepair = configpatch.Verify(plan.InstallRoot, rules) != nil
	}
	if configNeedsRepair {
		if err := configpatch.Apply(plan.InstallRoot, rules, backupRoot); err != nil {
			return state, fmt.Errorf("repatch changed LAN IP: %w", err)
		}
		if err := configpatch.Verify(plan.InstallRoot, rules); err != nil {
			return state, fmt.Errorf("verify repaired LAN configuration: %w", err)
		}
		state.LANIP = plan.LAN.Address
		state.PatchedKeys = len(rules)
		record("config-repatch", plan.InstallRoot, "update-detected-lan-ip", "completed", true, nil)
	}
	botRules := botEnvRules(manifest, plan.InstallRoot)
	if len(botRules) > 0 && configpatch.VerifyEnv(plan.InstallRoot, botRules) != nil {
		if err := configpatch.ApplyEnv(plan.InstallRoot, botRules, backupRoot); err != nil {
			return state, fmt.Errorf("repatch the bot configuration: %w", err)
		}
		if err := configpatch.VerifyEnv(plan.InstallRoot, botRules); err != nil {
			return state, fmt.Errorf("verify the repaired bot configuration: %w", err)
		}
		record("config-repatch-bot", plan.InstallRoot, "rebind-bot-to-server", "completed", true, nil)
	}
	state.PatchedBotKeys = len(botRules)
	state.OperationID = operationID
	state.JournalPath = journal.Path
	state.Status = "resuming"
	state.LastError = ""
	state.RecoveryAction = ""
	if err := writeJSONAtomic(statePath, state); err != nil {
		return state, err
	}
	return finishCommittedInstall(ctx, options, plan, manifest, state, wasComplete, record, logf, progress)
}

// patchedTargets are the files Setup rewrites after extraction. Their bytes no
// longer match the manifest, so repair may only restore them when missing.
var patchedTargets = buildPatchedTargets()

func buildPatchedTargets() map[string]struct{} {
	targets := make(map[string]struct{})
	for _, rule := range configpatch.LANRules("127.0.0.1") {
		targets[strings.ToLower(rule.RelativePath)] = struct{}{}
	}
	// Only the path matters here, so the bot env file is listed directly rather
	// than derived from credentials this package does not have yet.
	targets[strings.ToLower(configpatch.BotEnvRelativePath)] = struct{}{}
	return targets
}

// botEnvRules returns the bot configuration to enforce for this release, or
// nothing when the release was built without a bot directory.
func botEnvRules(manifest *release.Manifest, installRoot string) []configpatch.EnvRule {
	if !manifest.IncludesBot {
		return nil
	}
	accounts := manifest.Accounts.WithDefaults()
	return configpatch.BotEnvRules(installRoot, accounts.BotUser, accounts.BotPassword)
}

func payloadRepairMode(entry release.FileEntry) sfx.RepairMode {
	target := strings.ToLower(filepath.ToSlash(entry.Target))
	if target == "installerdata/database/jxaccount.sql" || strings.HasPrefix(target, "installerdata/packages/") {
		return sfx.RepairVerify
	}
	payloadRoots := []string{
		strings.ToLower(ClientTargetRoot) + "/",
		strings.ToLower(ServerTargetRoot) + "/",
		strings.ToLower(BotTargetRoot) + "/",
	}
	inPayload := false
	for _, root := range payloadRoots {
		if strings.HasPrefix(target, root) {
			inPayload = true
			break
		}
	}
	if !inPayload {
		return sfx.RepairSkip
	}
	if _, patched := patchedTargets[target]; patched {
		return sfx.RepairMissing
	}
	switch strings.ToLower(filepath.Ext(target)) {
	case ".exe", ".dll", ".pak", ".bat", ".cmd", ".ocx", ".ax", ".flt":
		return sfx.RepairVerify
	default:
		return sfx.RepairMissing
	}
}

func finishCommittedInstall(ctx context.Context, options Options, plan *Plan, manifest *release.Manifest, state *State, adoptCompleteDatabase bool, record recordFunc, logf func(string, ...any), progress func(percent int, stage, detail string)) (*State, error) {
	statePath := filepath.Join(plan.InstallRoot, "InstallerData", "install-state.json")
	fail := func(phase string, operationErr error) (*State, error) {
		state.Status = "recovery-required"
		state.LastError = operationErr.Error()
		state.RecoveryAction = "Rerun the same Setup.exe beside the matching ISO and select the same installation directory."
		_ = writeJSONAtomic(statePath, state)
		record(phase, plan.InstallRoot, "post-commit", "failed", true, operationErr)
		return state, fmt.Errorf("installation is committed and can be resumed safely; rerun the same release and directory: %w", operationErr)
	}
	if progress != nil {
		progress(68, "Install MySQL", "Installing or validating local MySQL 5.5.15")
	}
	checkImportSpace := func() error {
		remaining, err := AvailableBytes(plan.InstallRoot)
		if err != nil {
			return err
		}
		if remaining < minimumPostInstallFreeDiskBytes {
			return fmt.Errorf("free disk reserve fell below %d bytes: available=%d", minimumPostInstallFreeDiskBytes, remaining)
		}
		return nil
	}
	if !state.MySQL.DatabaseReady {
		packagePath := filepath.Join(plan.InstallRoot, filepath.FromSlash(manifest.MySQL.Target))
		diskEstimate, err := database.EstimateInstallDisk(packagePath, manifest.Database.Size)
		if err != nil {
			return fail("mysql-disk-preflight", err)
		}
		requiredFree, err := checkedAddDiskBytes(diskEstimate.TotalBytes, minimumPostInstallFreeDiskBytes)
		if err != nil {
			return fail("mysql-disk-preflight", err)
		}
		available, err := AvailableBytes(plan.InstallRoot)
		if err != nil {
			return fail("mysql-disk-preflight", err)
		}
		logf("MySQL disk preflight: runtime=%d data-template-copy=%d database-budget=%d reserve=%d available=%d", diskEstimate.RuntimeBytes, diskEstimate.DataTemplateBytes, diskEstimate.DatabaseBytes, minimumPostInstallFreeDiskBytes, available)
		if available < requiredFree {
			return fail("mysql-disk-preflight", fmt.Errorf("insufficient disk for MySQL and jxaccount: available=%d required=%d", available, requiredFree))
		}
		record("mysql-disk-preflight", plan.InstallRoot, "reserve-runtime-data-import", "completed", true, nil)
	}
	installedSQL := filepath.Join(plan.InstallRoot, filepath.FromSlash(manifest.Database.Target))
	mysqlResult, err := database.EnsureMySQL(ctx, database.MySQLOptions{
		InstallRoot: plan.InstallRoot, SQLPath: installedSQL, SQL: manifest.Database, Package: manifest.MySQL,
		Accounts:              manifest.Accounts,
		AdoptCompleteDatabase: adoptCompleteDatabase, CheckImportSpace: checkImportSpace, Logf: logf, Record: record,
	})
	if err != nil {
		return fail("mysql", err)
	}
	state.MySQL = mysqlResult
	state.Status = "database-ready"
	state.LastError = ""
	state.RecoveryAction = ""
	if err := writeJSONAtomic(statePath, state); err != nil {
		return fail("install-state", err)
	}
	if progress != nil {
		progress(94, "Create shortcuts", "Publishing Game and AutoPk shortcuts to the desktop")
	}
	shortcuts, err := CreateDesktopShortcuts(ctx, plan.InstallRoot, DesktopShortcuts(), logf)
	record("desktop-shortcuts", plan.InstallRoot, "create", operationStatus(err), true, err)
	if err != nil {
		return fail("desktop-shortcuts", err)
	}
	state.Shortcuts = shortcuts
	state.BotDiskWarning = plan.BotDiskWarning
	if plan.BotDiskWarning != "" {
		logf("WARNING: %s", plan.BotDiskWarning)
	}
	state.Status = "complete"
	if err := writeJSONAtomic(statePath, state); err != nil {
		return fail("install-state", err)
	}
	record("install-finish", plan.InstallRoot, "finish", "completed", true, nil)
	if progress != nil {
		progress(100, "Complete", "Client, Server, Bot, MySQL and jxaccount are ready")
	}
	return state, nil
}

func removeStagingWithRetry(stage string) error {
	var failures []error
	for attempt := 1; attempt <= 4; attempt++ {
		if err := os.RemoveAll(stage); err == nil || os.IsNotExist(err) {
			return nil
		} else {
			failures = append(failures, fmt.Errorf("remove staging directory %s (attempt %d): %w", stage, attempt, err))
		}
		if attempt < 4 {
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}
	}
	return errors.Join(failures...)
}

// The install-root mutex proves no live Setup owns these marked stages, even
// when the abandoned stage came from an older release.
func cleanupStaleStages(installRoot string) error {
	matches, err := filepath.Glob(installRoot + ".staging-*")
	if err != nil {
		return err
	}
	for _, stage := range matches {
		info, err := os.Lstat(stage)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unrecognized staging path preserved: %s", stage)
		}
		raw, err := os.ReadFile(filepath.Join(stage, ".kiemthedeployforge-staging.json"))
		if err != nil {
			return fmt.Errorf("stale staging directory has no valid ownership marker; preserving %s: %w", stage, err)
		}
		var owner stagingOwner
		if err := json.Unmarshal(raw, &owner); err != nil || owner.Product != "KiemTheDeployForge" || owner.ReleaseID == "" || !strings.EqualFold(filepath.Clean(owner.InstallRoot), filepath.Clean(installRoot)) {
			return fmt.Errorf("stale staging directory belongs to another operation; preserving %s", stage)
		}
		if err := removeStagingWithRetry(stage); err != nil {
			return err
		}
	}
	return nil
}

func removeCommittedStagingMarker(installRoot, releaseID string) error {
	markerPath := filepath.Join(installRoot, ".kiemthedeployforge-staging.json")
	raw, err := os.ReadFile(markerPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var owner stagingOwner
	if err := json.Unmarshal(raw, &owner); err != nil || owner.Product != "KiemTheDeployForge" || owner.ReleaseID != releaseID || !strings.EqualFold(filepath.Clean(owner.InstallRoot), filepath.Clean(installRoot)) {
		return fmt.Errorf("committed staging marker has invalid ownership; preserving %s", markerPath)
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func loadState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// ValidateInstallTarget rejects an installation directory that would swallow
// the release media it is being installed from.
//
// Choosing the folder that holds Setup.exe and the ISO is an easy mistake, and
// it cannot work: the commit step renames the staging directory onto the
// install root, which requires that root not to exist yet. Detecting it here
// produces an explanation instead of a confusing resume failure later.
//
// Installing into a subdirectory of the media folder is fine and stays allowed.
func ValidateInstallTarget(installRoot, setupPath string) error {
	root, err := validateInstallRoot(installRoot)
	if err != nil {
		return err
	}
	absoluteSetup, err := filepath.Abs(setupPath)
	if err != nil {
		return err
	}
	mediaDirectory := filepath.Dir(absoluteSetup)
	if pathContains(root, mediaDirectory) {
		return fmt.Errorf(
			"the installation directory cannot be %s, because %s holds the Setup.exe and the ISO being installed from; "+
				"choose a separate directory such as %s",
			root, mediaDirectory, filepath.Join(root, "KiemTheServer"))
	}
	return nil
}

// pathContains reports whether candidate is root itself or lies beneath it.
func pathContains(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func validateInstallRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("install root is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(absolute)
	volume := filepath.VolumeName(clean)
	if volume == "" || strings.EqualFold(clean, volume+string(filepath.Separator)) || strings.HasPrefix(clean, `\\`) {
		return "", fmt.Errorf("unsafe install root %q", value)
	}
	return clean, nil
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temp := path + fmt.Sprintf(".tmp-%d", os.Getpid())
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		_ = os.Remove(temp)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(temp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.Rename(temp, path)
	}
	return winfile.Replace(temp, path)
}

func newOperationID() (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random), nil
}

func operationStatus(err error) string {
	if err != nil {
		return "failed"
	}
	return "completed"
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func requiredInstallBytes(payloadBytes, sqlBytes int64) (int64, error) {
	if payloadBytes < 0 || sqlBytes < 0 {
		return 0, fmt.Errorf("installation byte count is negative")
	}
	if sqlBytes > math.MaxInt64/plannedSQLExpansionMultiplier {
		return 0, fmt.Errorf("SQL disk estimate overflow")
	}
	return checkedAddInt64(payloadBytes, payloadBytes/5, sqlBytes*plannedSQLExpansionMultiplier, plannedMySQLExpansionAllowance, installDiskHeadroom)
}

func checkedAddInt64(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, fmt.Errorf("installation disk requirement overflow")
		}
		total += value
	}
	return total, nil
}

func checkedAddDiskBytes(values ...uint64) (uint64, error) {
	var total uint64
	for _, value := range values {
		if total > math.MaxUint64-value {
			return 0, fmt.Errorf("installation disk requirement overflow")
		}
		total += value
	}
	return total, nil
}
