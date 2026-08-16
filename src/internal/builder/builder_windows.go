package builder

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kiemthedeployforge/internal/configpatch"
	"kiemthedeployforge/internal/database"
	"kiemthedeployforge/internal/install"
	"kiemthedeployforge/internal/iso"
	"kiemthedeployforge/internal/release"
	"kiemthedeployforge/internal/sfx"
	"kiemthedeployforge/internal/winfile"
)

type Options struct {
	ClientPath string
	ServerPath string
	// BotPath is optional. Leaving it empty builds a release without a bot:
	// no bot files are packaged, Setup skips the bot configuration step and
	// the 20 GiB system drive reserve no longer applies.
	BotPath    string
	SQLPath    string
	OutputPath string
	SkipISO    bool
	SetupStub  []byte
	// Accounts overrides the MySQL credentials baked into the release. A zero
	// value keeps the stock root/1234 and bot_writer/1234.
	Accounts release.Accounts
	Progress func(percent int, stage, detail string)
}

type Result struct {
	SetupPath     string
	SetupSHA256   string
	PayloadPath   string `json:",omitempty"`
	PayloadSHA256 string `json:",omitempty"`
	ISOPath       string
	ISOSHA256     string
	ReleaseID     string
	Files         int
	Bytes         int64
}

type sourceFile struct {
	Source string
	Entry  release.FileEntry
}

const buildDiskHeadroom = uint64(2 * 1024 * 1024 * 1024)

func Build(ctx context.Context, options Options) (*Result, error) {
	report := options.Progress
	if report == nil {
		report = func(int, string, string) {}
	}
	client, err := existingDirectory(options.ClientPath, "Client")
	if err != nil {
		return nil, err
	}
	server, err := existingDirectory(options.ServerPath, "Server")
	if err != nil {
		return nil, err
	}
	// An empty bot path is a deliberate choice, not a mistake: the bot is an
	// optional component of a release.
	bot := ""
	includesBot := strings.TrimSpace(options.BotPath) != ""
	if includesBot {
		bot, err = existingDirectory(options.BotPath, "Bot")
		if err != nil {
			return nil, err
		}
	}
	accounts := options.Accounts.WithDefaults()
	if err := accounts.Validate(); err != nil {
		return nil, fmt.Errorf("MySQL accounts: %w", err)
	}
	sqlPath, err := existingFile(options.SQLPath, "jxaccount SQL")
	if err != nil {
		return nil, err
	}
	sqlPath, err = filepath.EvalSymlinks(sqlPath)
	if err != nil {
		return nil, fmt.Errorf("resolve jxaccount SQL path: %w", err)
	}
	if !strings.EqualFold(filepath.Base(sqlPath), "jxaccount.sql") {
		return nil, fmt.Errorf("database input must be named jxaccount.sql: %s", sqlPath)
	}
	sources := []struct {
		label string
		path  string
	}{{"Client", client}, {"Server", server}}
	if includesBot {
		sources = append(sources, struct {
			label string
			path  string
		}{"Bot", bot})
	}
	for i, left := range sources {
		for _, right := range sources[i+1:] {
			if pathContains(left.path, right.path) || pathContains(right.path, left.path) {
				return nil, fmt.Errorf("%s and %s directories must not overlap", left.label, right.label)
			}
		}
		if pathContains(left.path, sqlPath) {
			return nil, fmt.Errorf("jxaccount.sql must be outside the %s directory to prevent duplicate payload bytes", left.label)
		}
	}
	rawOutput := strings.TrimSpace(options.OutputPath)
	if rawOutput == "" {
		return nil, fmt.Errorf("release output directory is required")
	}
	output, err := filepath.Abs(rawOutput)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return nil, err
	}
	output, err = filepath.EvalSymlinks(output)
	if err != nil {
		return nil, err
	}
	if _, err := install.RequireFixedNTFS(output, "release output directory"); err != nil {
		return nil, err
	}
	for _, source := range sources {
		if pathContains(source.path, output) {
			return nil, fmt.Errorf("output directory cannot be inside %s", source.path)
		}
	}
	outputLock, err := acquireOutputBuildLock(output)
	if err != nil {
		return nil, err
	}
	defer outputLock.Close()
	if err := cleanupAbandonedBuildOutputs(output); err != nil {
		return nil, fmt.Errorf("clean abandoned package build: %w", err)
	}
	if err := cleanupStaleBuildArtifacts(output, time.Now()); err != nil {
		return nil, fmt.Errorf("clean stale package build artifacts: %w", err)
	}
	setupPath := filepath.Join(output, "Setup.exe")
	payloadPath := filepath.Join(output, sfx.PayloadFileName)
	isoPath := filepath.Join(output, "KiemTheServer-Offline.iso")
	readmePath := filepath.Join(output, "README.txt")
	for _, path := range []string{setupPath, payloadPath, readmePath} {
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("output already exists: %s", path)
		}
	}
	if !options.SkipISO {
		if _, err := os.Stat(isoPath); err == nil {
			return nil, fmt.Errorf("output already exists: %s", isoPath)
		}
	}
	if err := writeActiveBuildMarker(output); err != nil {
		return nil, err
	}
	createdOutputs := make([]string, 0, 4)
	buildComplete := false
	defer func() {
		if buildComplete {
			return
		}
		cleanupComplete := true
		for _, path := range createdOutputs {
			if err := removeBuildFileWithRetry(path); err != nil {
				cleanupComplete = false
			}
		}
		if cleanupComplete {
			_ = removeActiveBuildMarker(output)
		}
	}()
	if len(options.SetupStub) < 1024*1024 || string(options.SetupStub[:2]) != "MZ" {
		return nil, fmt.Errorf("embedded Setup GUI stub is invalid")
	}
	if err := validateRequiredFiles(client, server, bot, accounts); err != nil {
		return nil, err
	}

	sqlInfo, err := os.Stat(sqlPath)
	if err != nil {
		return nil, err
	}
	report(1, "Scan and hash", "Enumerating Client, Server and Bot")
	clientPaths, clientDirectories, clientBytes, err := enumerateTree(ctx, client)
	if err != nil {
		return nil, err
	}
	serverPaths, serverDirectories, serverBytes, err := enumerateTree(ctx, server)
	if err != nil {
		return nil, err
	}
	var botPaths, botDirectories []string
	var botBytes int64
	if includesBot {
		botPaths, botDirectories, botBytes, err = enumerateTree(ctx, bot)
		if err != nil {
			return nil, err
		}
	}
	estimatedPayloadBytes, err := addPayloadBytes(clientBytes, serverBytes, botBytes, sqlInfo.Size(), database.PinnedMySQLSize)
	if err != nil {
		return nil, err
	}
	required, err := requiredBuildBytes(estimatedPayloadBytes)
	if err != nil {
		return nil, err
	}
	available, err := install.AvailableBytes(output)
	if err != nil {
		return nil, err
	}
	report(2, "Disk preflight", fmt.Sprintf("Need %.2f GiB peak; %.2f GiB available on the output volume", bytesToGiB(required), bytesToGiB(available)))
	if available < required {
		return nil, fmt.Errorf("insufficient output disk space before hashing: available=%d required=%d", available, required)
	}
	hashTotal, err := addPayloadBytes(clientBytes, serverBytes, botBytes, sqlInfo.Size())
	if err != nil {
		return nil, err
	}
	var hashed int64
	files := make([]sourceFile, 0, len(clientPaths)+len(serverPaths)+len(botPaths)+2)
	directories := make([]release.DirectoryEntry, 0, len(clientDirectories)+len(serverDirectories)+len(botDirectories))
	appendDirectories := func(root, targetRoot string, paths []string) error {
		for _, path := range paths {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := targetRoot
			if relative != "." {
				target = filepath.Join(targetRoot, relative)
			}
			attrs, err := winfile.Attributes(path)
			if err != nil {
				return err
			}
			directories = append(directories, release.DirectoryEntry{
				Target: filepath.ToSlash(target), Attributes: attrs,
				LastWriteTimeUTC: info.ModTime().UTC().Format(time.RFC3339Nano),
			})
		}
		return nil
	}
	appendTree := func(root, mediaRoot, targetRoot string, paths []string) error {
		for _, path := range paths {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			hash, err := hashFile(ctx, path, func(delta int64) {
				hashed += delta
				percent := 3 + int(float64(hashed)/float64(max64(hashTotal, 1))*22)
				report(percent, "Scan and hash", filepath.Base(path))
			})
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			attrs, err := winfile.Attributes(path)
			if err != nil {
				return err
			}
			files = append(files, sourceFile{Source: path, Entry: release.FileEntry{
				Path: filepath.ToSlash(filepath.Join(mediaRoot, relative)), Target: filepath.ToSlash(filepath.Join(targetRoot, relative)),
				Size: info.Size(), SHA256: hash, Attributes: attrs, LastWriteTimeUTC: info.ModTime().UTC().Format(time.RFC3339Nano),
			}})
		}
		return nil
	}
	if err := appendTree(client, "payload/Client", "Client", clientPaths); err != nil {
		return nil, err
	}
	if err := appendTree(server, "payload/"+configpatch.ServerTargetRoot, configpatch.ServerTargetRoot, serverPaths); err != nil {
		return nil, err
	}
	if includesBot {
		if err := appendTree(bot, "payload/"+install.BotTargetRoot, install.BotTargetRoot, botPaths); err != nil {
			return nil, err
		}
	}
	if err := appendDirectories(client, "Client", clientDirectories); err != nil {
		return nil, err
	}
	if err := appendDirectories(server, configpatch.ServerTargetRoot, serverDirectories); err != nil {
		return nil, err
	}
	if includesBot {
		if err := appendDirectories(bot, install.BotTargetRoot, botDirectories); err != nil {
			return nil, err
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.ToLower(directories[i].Target) < strings.ToLower(directories[j].Target)
	})
	sqlHash, err := hashFile(ctx, sqlPath, func(delta int64) {
		hashed += delta
		report(25, "Scan and hash", "Validating jxaccount SQL")
	})
	if err != nil {
		return nil, err
	}
	if _, err := database.ValidateSQL(sqlPath, sqlInfo.Size(), sqlHash); err != nil {
		return nil, err
	}
	sqlAttrs, err := winfile.Attributes(sqlPath)
	if err != nil {
		return nil, err
	}
	sqlEntry := release.FileEntry{Path: "database/jxaccount.sql", Target: "InstallerData/database/jxaccount.sql", Size: sqlInfo.Size(), SHA256: sqlHash, Attributes: sqlAttrs, LastWriteTimeUTC: sqlInfo.ModTime().UTC().Format(time.RFC3339Nano)}
	files = append(files, sourceFile{Source: sqlPath, Entry: sqlEntry})

	report(26, "MySQL package", "Locate or download pinned MySQL 5.5.15 Win32")
	mysqlPath, cleanupMySQL, err := getPinnedMySQL(ctx, output, func(done, total int64) {
		percent := 26
		if total > 0 {
			percent += int(float64(done) / float64(total) * 9)
		}
		report(percent, "MySQL package", fmt.Sprintf("%d / %d bytes", done, total))
	})
	if err != nil {
		return nil, err
	}
	defer cleanupMySQL()
	mysqlInfo, err := os.Stat(mysqlPath)
	if err != nil {
		return nil, err
	}
	mysqlAttrs, err := winfile.Attributes(mysqlPath)
	if err != nil {
		return nil, err
	}
	mysqlEntry := release.FileEntry{Path: "prerequisites/" + database.PinnedMySQLName, Target: "InstallerData/packages/" + database.PinnedMySQLName, Size: mysqlInfo.Size(), SHA256: database.PinnedMySQLSHA256, Attributes: mysqlAttrs, LastWriteTimeUTC: mysqlInfo.ModTime().UTC().Format(time.RFC3339Nano)}
	files = append(files, sourceFile{Source: mysqlPath, Entry: mysqlEntry})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Entry.Path) < strings.ToLower(files[j].Entry.Path)
	})

	var payloadBytes int64
	manifestFiles := make([]release.FileEntry, len(files))
	for i, file := range files {
		manifestFiles[i] = file.Entry
		payloadBytes, err = addPayloadBytes(payloadBytes, file.Entry.Size)
		if err != nil {
			return nil, err
		}
	}
	if payloadBytes != estimatedPayloadBytes {
		return nil, fmt.Errorf("payload inventory changed while preparing build: estimated=%d actual=%d", estimatedPayloadBytes, payloadBytes)
	}
	releaseID := "KiemTheServer-" + time.Now().UTC().Format("20060102-150405")
	manifest := release.Manifest{
		FormatVersion: 1, Product: "KiemTheDeployForge", ReleaseID: releaseID,
		CreatedUTC: time.Now().UTC().Format(time.RFC3339Nano), PayloadBytes: payloadBytes, Directories: directories, Files: manifestFiles,
		MySQL:    release.MySQLArtifact{Version: "5.5.15-win32", Path: mysqlEntry.Path, Target: mysqlEntry.Target, Size: database.PinnedMySQLSize, SHA256: database.PinnedMySQLSHA256, MD5: database.PinnedMySQLMD5, Source: database.PinnedMySQLURL},
		Database: release.SQLArtifact{Path: sqlEntry.Path, Target: sqlEntry.Target, Size: sqlEntry.Size, SHA256: sqlEntry.SHA256},
		Accounts: accounts, IncludesBot: includesBot,
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("release manifest is invalid: %w", err)
	}
	manifestRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestRaw = append(manifestRaw, '\n')

	report(36, "Build payload package", "Writing ZIP64 payload for the offline ISO")
	payloadHash, err := writeArchive(ctx, payloadPath, nil, manifestRaw, files, payloadBytes, func(done int64, detail string) {
		percent := 36 + int(float64(done)/float64(max64(payloadBytes, 1))*35)
		report(percent, "Build payload package", detail)
	})
	if err != nil {
		return nil, err
	}
	createdOutputs = append(createdOutputs, payloadPath)
	report(72, "Build Setup bootstrap", "Writing a small runnable Setup.exe with the release manifest pin")
	setupHash, err := writeArchive(ctx, setupPath, options.SetupStub, manifestRaw, nil, 0, nil)
	if err != nil {
		return nil, err
	}
	createdOutputs = append(createdOutputs, setupPath)
	report(74, "Verify payload package", "Checking ZIP64 central directory and release manifest")
	pack, err := sfx.Open(payloadPath)
	if err != nil {
		return nil, err
	}
	bootstrap, err := sfx.OpenManifest(setupPath)
	if err != nil {
		pack.Close()
		return nil, err
	}
	if !strings.EqualFold(pack.ManifestSHA256, bootstrap.ManifestSHA256) {
		bootstrap.Close()
		pack.Close()
		return nil, fmt.Errorf("Setup.exe release pin does not match %s", sfx.PayloadFileName)
	}
	bootstrap.Close()
	if options.SkipISO {
		err = pack.VerifyAll(ctx, func(progress sfx.Progress) {
			percent := 75 + int(float64(progress.CopiedBytes)/float64(max64(progress.TotalBytes, 1))*15)
			report(percent, "Verify payload package", progress.Path)
		})
	}
	pack.Close()
	if err != nil {
		return nil, err
	}
	installedComponents := install.ClientTargetRoot + " and " + configpatch.ServerTargetRoot
	botNote := ""
	if includesBot {
		installedComponents = install.ClientTargetRoot + ", " + configpatch.ServerTargetRoot + " and " + install.BotTargetRoot
		botNote = "The bot needs at least 20 GiB free on drive C:.\r\n"
	}
	readme := "KIEM THE SERVER OFFLINE SETUP x64\r\n\r\n" + install.AuthorCredit + "\r\n\r\n" +
		"Run Setup.exe as Administrator. Setup automatically reads Payload.ktpkg from this ISO, detects the destination machine LAN IPv4, installs " + installedComponents + ", creates the desktop shortcuts, then imports jxaccount into the local MySQL 5.5.15 service. Setup.exe is x64; MySQL 5.5.15 Win32 is only the isolated legacy database runtime and does not define the Setup or GameServer architecture.\r\n\r\n" +
		"MySQL accounts on the destination machine: " + database.RootUser + "/" + accounts.RootPassword + " and " + accounts.BotUser + "/" + accounts.BotPassword + " (full rights on " + database.DatabaseName + ").\r\n" +
		botNote
	if err := writeNewOutput(readmePath, []byte(readme)); err != nil {
		return nil, err
	}
	createdOutputs = append(createdOutputs, readmePath)
	result := &Result{SetupPath: setupPath, SetupSHA256: setupHash, PayloadPath: payloadPath, PayloadSHA256: payloadHash, ReleaseID: releaseID, Files: len(files), Bytes: payloadBytes}
	if !options.SkipISO {
		payloadInfo, err := os.Stat(payloadPath)
		if err != nil {
			return nil, err
		}
		isoRequired, err := requiredISOBytes(payloadInfo.Size())
		if err != nil {
			return nil, err
		}
		isoAvailable, err := install.AvailableBytes(output)
		if err != nil {
			return nil, err
		}
		report(77, "Disk preflight", fmt.Sprintf("Need %.2f GiB free to create ISO; %.2f GiB remains", bytesToGiB(isoRequired), bytesToGiB(isoAvailable)))
		if isoAvailable < isoRequired {
			return nil, fmt.Errorf("insufficient output disk space before ISO creation: available=%d required=%d", isoAvailable, isoRequired)
		}
		manifestSum := sha256.Sum256(manifestRaw)
		report(78, "Build ISO UDF", "Creating ISO with Setup.exe and the external payload package")
		isoHash, err := iso.Create(ctx, iso.Options{
			SetupPath: setupPath, SetupSHA256: setupHash,
			PayloadPath: payloadPath, PayloadSHA256: payloadHash,
			Manifest: manifestRaw, ManifestSHA256: hex.EncodeToString(manifestSum[:]),
			ReadmePath: readmePath, OutputPath: isoPath,
		}, func(percent int, stage, detail string) {
			report(78+percent*22/100, stage, detail)
		})
		if err != nil {
			return nil, err
		}
		result.ISOPath = isoPath
		result.ISOSHA256 = isoHash
		createdOutputs = append(createdOutputs, isoPath)
		if err := removeBuildFileWithRetry(payloadPath); err != nil {
			return nil, fmt.Errorf("remove temporary payload package after ISO commit: %w", err)
		}
		if err := removeBuildFileWithRetry(readmePath); err != nil {
			return nil, fmt.Errorf("remove temporary README after ISO commit: %w", err)
		}
		result.PayloadPath = ""
	}
	report(100, "Complete", "Small Setup.exe and offline payload ISO are ready")
	if err := removeActiveBuildMarker(output); err != nil {
		return nil, fmt.Errorf("remove active build marker: %w", err)
	}
	buildComplete = true
	return result, nil
}

func writeArchive(ctx context.Context, outputPath string, prefix, manifestRaw []byte, files []sourceFile, payloadBytes int64, report func(int64, string)) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(outputPath), "."+filepath.Base(outputPath)+"-*.building")
	if err != nil {
		return "", err
	}
	temp := file.Name()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		_ = os.Remove(temp)
		return "", err
	}
	keep := true
	defer func() {
		file.Close()
		if keep {
			_ = os.Remove(temp)
		}
	}()
	archiveHash := sha256.New()
	combined := io.MultiWriter(file, archiveHash)
	if _, err := combined.Write(prefix); err != nil {
		return "", err
	}
	writer := zip.NewWriter(combined)
	writer.SetOffset(int64(len(prefix)))
	manifestHeader := &zip.FileHeader{Name: sfx.ManifestPath, Method: zip.Store, Modified: time.Now().UTC()}
	manifestOutput, err := writer.CreateHeader(manifestHeader)
	if err != nil {
		return "", err
	}
	if _, err := manifestOutput.Write(manifestRaw); err != nil {
		return "", err
	}
	var written int64
	buffer := make([]byte, 4*1024*1024)
	for _, source := range files {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		stamp, err := time.Parse(time.RFC3339Nano, source.Entry.LastWriteTimeUTC)
		if err != nil {
			return "", err
		}
		header := &zip.FileHeader{Name: source.Entry.Path, Method: zip.Store, Modified: stamp}
		header.CreatorVersion = 0
		header.ExternalAttrs = source.Entry.Attributes
		entryOutput, err := writer.CreateHeader(header)
		if err != nil {
			return "", err
		}
		input, err := os.Open(source.Source)
		if err != nil {
			return "", err
		}
		hash := sha256.New()
		var fileBytes int64
		for {
			select {
			case <-ctx.Done():
				input.Close()
				return "", ctx.Err()
			default:
			}
			read, readErr := input.Read(buffer)
			if read > 0 {
				if _, err := entryOutput.Write(buffer[:read]); err != nil {
					input.Close()
					return "", err
				}
				_, _ = hash.Write(buffer[:read])
				fileBytes += int64(read)
				written += int64(read)
				if report != nil {
					report(written, source.Entry.Path)
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				input.Close()
				return "", readErr
			}
		}
		input.Close()
		if fileBytes != source.Entry.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), source.Entry.SHA256) {
			return "", fmt.Errorf("source changed while building: %s", source.Source)
		}
	}
	if written != payloadBytes {
		return "", fmt.Errorf("archive payload byte count mismatch")
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temp, outputPath); err != nil {
		return "", err
	}
	keep = false
	return hex.EncodeToString(archiveHash.Sum(nil)), nil
}

func writeNewOutput(path string, content []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.building")
	if err != nil {
		return err
	}
	temp := file.Name()
	keep := true
	defer func() {
		_ = file.Close()
		if keep {
			_ = os.Remove(temp)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	keep = false
	return nil
}

func getPinnedMySQL(ctx context.Context, outputRoot string, report func(done, total int64)) (string, func(), error) {
	noCleanup := func() {}
	var cacheErr error
	if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
		cacheRoot := filepath.Join(localAppData, "KiemTheDeployForge", "cache")
		cachePath := filepath.Join(cacheRoot, database.PinnedMySQLName)
		if err := cleanupStaleDownloadArtifacts(cacheRoot, time.Now()); err != nil {
			cacheErr = fmt.Errorf("clean LOCALAPPDATA cache: %w", err)
		} else if ok, verifyErr := verifyMySQL(ctx, cachePath); ok {
			if report != nil {
				report(database.PinnedMySQLSize, database.PinnedMySQLSize)
			}
			return cachePath, noCleanup, nil
		} else if verifyErr != nil && ctx.Err() != nil {
			return "", noCleanup, ctx.Err()
		} else if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
			cacheErr = fmt.Errorf("prepare LOCALAPPDATA cache: %w", err)
		} else {
			_ = os.Remove(cachePath)
			if err := requireDownloadSpace(cacheRoot); err != nil {
				cacheErr = err
			} else if err := downloadPinnedFile(ctx, http.DefaultClient, database.PinnedMySQLURL, cachePath, database.PinnedMySQLSize, database.PinnedMySQLSHA256, report); err == nil {
				return cachePath, noCleanup, nil
			} else {
				cacheErr = err
			}
		}
	}

	// Keep the fallback on the selected output volume so a small system drive
	// cannot prevent building a release on a large data drive.
	fallbackRoot, err := os.MkdirTemp(outputRoot, mysqlCachePrefix)
	if err != nil {
		return "", noCleanup, fmt.Errorf("prepare output cache: %w", err)
	}
	keepFallback := true
	defer func() {
		if keepFallback {
			_ = os.RemoveAll(fallbackRoot)
		}
	}()
	if err := os.WriteFile(filepath.Join(fallbackRoot, mysqlCacheMarkerName), []byte(mysqlCacheMarkerContent), 0o600); err != nil {
		return "", noCleanup, fmt.Errorf("mark output cache: %w", err)
	}
	if err := requireDownloadSpace(fallbackRoot); err != nil {
		if cacheErr != nil {
			return "", noCleanup, fmt.Errorf("LOCALAPPDATA cache unavailable (%v); output cache unavailable: %w", cacheErr, err)
		}
		return "", noCleanup, err
	}
	fallbackPath := filepath.Join(fallbackRoot, database.PinnedMySQLName)
	if err := downloadPinnedFile(ctx, http.DefaultClient, database.PinnedMySQLURL, fallbackPath, database.PinnedMySQLSize, database.PinnedMySQLSHA256, report); err != nil {
		if cacheErr != nil {
			return "", noCleanup, fmt.Errorf("LOCALAPPDATA cache unavailable (%v); output cache download failed: %w", cacheErr, err)
		}
		return "", noCleanup, err
	}
	cleanup := func() {
		_ = os.RemoveAll(fallbackRoot)
	}
	keepFallback = false
	return fallbackPath, cleanup, nil
}

func requireDownloadSpace(path string) error {
	available, err := install.AvailableBytes(path)
	if err != nil {
		return fmt.Errorf("check cache disk space: %w", err)
	}
	const headroom = uint64(16 * 1024 * 1024)
	required := uint64(database.PinnedMySQLSize) + headroom
	if available < required {
		return fmt.Errorf("cache disk is too full: available=%d required=%d", available, required)
	}
	return nil
}

func downloadPinnedFile(ctx context.Context, client *http.Client, sourceURL, destination string, expectedSize int64, expectedSHA256 string, report func(done, total int64)) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download pinned file: HTTP %s", response.Status)
	}
	if response.ContentLength >= 0 && response.ContentLength != expectedSize {
		return fmt.Errorf("download Content-Length mismatch: got %d want %d", response.ContentLength, expectedSize)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+"-*.download")
	if err != nil {
		return err
	}
	temp := output.Name()
	keepTemp := true
	defer func() {
		_ = output.Close()
		if keepTemp {
			_ = os.Remove(temp)
		}
	}()
	buffer := make([]byte, 1024*1024)
	var done int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			if done+int64(read) > expectedSize {
				return fmt.Errorf("download exceeded pinned size %d", expectedSize)
			}
			if _, err := output.Write(buffer[:read]); err != nil {
				return err
			}
			done += int64(read)
			if report != nil {
				report(done, expectedSize)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if done != expectedSize {
		return fmt.Errorf("download size mismatch: got %d want %d", done, expectedSize)
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if ok, err := verifyPinnedFile(ctx, temp, expectedSize, expectedSHA256); !ok {
		return fmt.Errorf("downloaded file failed pinned verification: %w", err)
	}
	if err := os.Rename(temp, destination); err != nil {
		if ok, _ := verifyPinnedFile(ctx, destination, expectedSize, expectedSHA256); ok {
			_ = os.Remove(temp)
			keepTemp = false
			return nil
		}
		return err
	}
	keepTemp = false
	return nil
}

func verifyMySQL(ctx context.Context, path string) (bool, error) {
	return verifyPinnedFile(ctx, path, database.PinnedMySQLSize, database.PinnedMySQLSHA256)
}

func verifyPinnedFile(ctx context.Context, path string, expectedSize int64, expectedSHA256 string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Size() != expectedSize {
		return false, fmt.Errorf("size mismatch: got %d want %d", info.Size(), expectedSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	sha := sha256.New()
	buffer := make([]byte, 1024*1024)
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = sha.Write(buffer[:read])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return false, readErr
		}
	}
	if !strings.EqualFold(hex.EncodeToString(sha.Sum(nil)), expectedSHA256) {
		return false, fmt.Errorf("SHA-256 mismatch")
	}
	return true, nil
}

func enumerateTree(ctx context.Context, root string) ([]string, []string, int64, error) {
	var paths []string
	var directoryPaths []string
	var totalBytes int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if walkErr != nil {
			return walkErr
		}
		attrs, err := winfile.Attributes(path)
		if err != nil {
			return err
		}
		if attrs&0x400 != 0 {
			return fmt.Errorf("reparse point is not supported: %s", path)
		}
		if info.IsDir() {
			directoryPaths = append(directoryPaths, path)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported source entry: %s", path)
		}
		paths = append(paths, path)
		totalBytes += info.Size()
		return nil
	})
	sort.Slice(paths, func(i, j int) bool { return strings.ToLower(paths[i]) < strings.ToLower(paths[j]) })
	sort.Slice(directoryPaths, func(i, j int) bool { return strings.ToLower(directoryPaths[i]) < strings.ToLower(directoryPaths[j]) })
	return paths, directoryPaths, totalBytes, err
}

func validateRequiredFiles(client, server, bot string, accounts release.Accounts) error {
	// Server start/stop batch files stay the operator's own business: the user
	// opens the installed Server directory and runs them by hand, so Setup
	// never depends on any particular name being present.
	required := []string{
		filepath.Join(client, install.ClientGameShortcutTarget),
		filepath.Join(client, install.ClientBotShortcutTarget),
		filepath.Join(client, "user", "uicommon.ini"),
		filepath.Join(client, "user", "serverlistdebug.ini"),
		filepath.Join(client, "AutoPk", "serverlist.ini"),
	}
	if bot != "" {
		required = append(required,
			filepath.Join(bot, install.BotExecutableName),
			filepath.Join(bot, install.BotEnvName),
		)
	}
	for i := 1; i <= configpatch.GameServerCount; i++ {
		required = append(required,
			filepath.Join(server, "Gameserver", fmt.Sprintf("GS%d.exe", i)),
			filepath.Join(server, "Gameserver", fmt.Sprintf("GS%dservercfg.ini", i)),
		)
	}
	for _, path := range required {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("missing required file: %s", path)
		}
	}
	serverPrefix := configpatch.ServerTargetRoot + "/"
	for _, rule := range configpatch.LANRules("127.0.0.1") {
		var path string
		switch {
		case strings.HasPrefix(rule.RelativePath, "Client/"):
			path = filepath.Join(client, filepath.FromSlash(strings.TrimPrefix(rule.RelativePath, "Client/")))
		case strings.HasPrefix(rule.RelativePath, serverPrefix):
			path = filepath.Join(server, filepath.FromSlash(strings.TrimPrefix(rule.RelativePath, serverPrefix)))
		default:
			return fmt.Errorf("unsupported configuration target: %s", rule.RelativePath)
		}
		if err := configpatch.ValidateTarget(path, rule.Section, rule.Key); err != nil {
			return fmt.Errorf("invalid configuration target %s [%s] %s: %w", path, rule.Section, rule.Key, err)
		}
	}
	if bot == "" {
		return nil
	}
	botPrefix := install.BotTargetRoot + "/"
	for _, rule := range configpatch.BotEnvRules(`C:\KiemTheServer`, accounts.BotUser, accounts.BotPassword) {
		if !strings.HasPrefix(rule.RelativePath, botPrefix) {
			return fmt.Errorf("unsupported bot configuration target: %s", rule.RelativePath)
		}
		path := filepath.Join(bot, filepath.FromSlash(strings.TrimPrefix(rule.RelativePath, botPrefix)))
		if err := configpatch.ValidateEnvTarget(path, rule.Key); err != nil {
			return fmt.Errorf("invalid bot configuration target %s %s: %w", path, rule.Key, err)
		}
	}
	return nil
}

func existingDirectory(path, label string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("%s directory is invalid: %s", label, path)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s directory is invalid: %s", label, path)
	}
	return filepath.Clean(canonical), nil
}

func existingFile(path, label string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("%s file is invalid: %s", label, path)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s file is invalid: %s", label, path)
	}
	return filepath.Clean(canonical), nil
}

func pathContains(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func removeBuildFileWithRetry(path string) error {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		err := os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if attempt < 7 {
			time.Sleep(time.Duration(25*(1<<attempt)) * time.Millisecond)
		}
	}
	return lastErr
}

func hashFile(ctx context.Context, path string, report func(delta int64)) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 4*1024*1024)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			if report != nil {
				report(int64(read))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func addPayloadBytes(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, fmt.Errorf("payload byte count overflow")
		}
		total += value
	}
	return total, nil
}

func requiredBuildBytes(payloadBytes int64) (uint64, error) {
	payload, err := payloadBytesAsUint64(payloadBytes)
	if err != nil {
		return 0, err
	}
	return addDiskBytes(payload, payload, payload/5, buildDiskHeadroom)
}

func requiredISOBytes(payloadPackageBytes int64) (uint64, error) {
	payload, err := payloadBytesAsUint64(payloadPackageBytes)
	if err != nil {
		return 0, err
	}
	return addDiskBytes(payload, payload/10, buildDiskHeadroom)
}

func payloadBytesAsUint64(value int64) (uint64, error) {
	if value < 0 {
		return 0, fmt.Errorf("payload byte count is negative")
	}
	return uint64(value), nil
}

func addDiskBytes(values ...uint64) (uint64, error) {
	var total uint64
	for _, value := range values {
		if total > math.MaxUint64-value {
			return 0, fmt.Errorf("disk requirement overflow")
		}
		total += value
	}
	return total, nil
}

func bytesToGiB(value uint64) float64 {
	return float64(value) / (1024 * 1024 * 1024)
}
