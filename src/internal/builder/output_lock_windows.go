package builder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"kiemthedeployforge/internal/database"
	"kiemthedeployforge/internal/iso"
)

const (
	activeBuildMarkerName    = ".kiemthedeployforge-build-in-progress.json"
	staleDownloadArtifactAge = 24 * time.Hour
	mysqlCachePrefix         = ".mysql-cache-"
	mysqlCacheMarkerName     = ".kiemthedeployforge-mysql-cache"
	mysqlCacheMarkerContent  = "KiemTheDeployForge\r\nkind=mysql-cache\r\n"
)

type activeBuildMarker struct {
	FormatVersion int    `json:"formatVersion"`
	Product       string `json:"product"`
	OutputPath    string `json:"outputPath"`
}

type outputBuildLock struct {
	release chan struct{}
	done    chan struct{}
	once    sync.Once
	errMu   sync.Mutex
	err     error
}

type outputBuildLockResult struct {
	lock *outputBuildLock
	err  error
}

func acquireOutputBuildLock(output string) (*outputBuildLock, error) {
	// Payload and ISO creation can consume many gigabytes. Serialize every
	// package build on the machine, even when callers choose different outputs.
	name := `Global\KiemTheDeployForge-PackageBuild`
	result := make(chan outputBuildLockResult, 1)
	lock := &outputBuildLock{release: make(chan struct{}), done: make(chan struct{})}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		namePointer, err := windows.UTF16PtrFromString(name)
		if err != nil {
			result <- outputBuildLockResult{err: err}
			return
		}
		handle, err := windows.CreateMutex(nil, false, namePointer)
		if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			result <- outputBuildLockResult{err: fmt.Errorf("create output build mutex: %w", err)}
			return
		}
		event, waitErr := windows.WaitForSingleObject(handle, 0)
		if waitErr != nil || (event != windows.WAIT_OBJECT_0 && event != windows.WAIT_ABANDONED) {
			_ = windows.CloseHandle(handle)
			if event == uint32(windows.WAIT_TIMEOUT) {
				result <- outputBuildLockResult{err: fmt.Errorf("another KiemTheDeployForge package build is already running; wait for it to finish before building %s", output)}
				return
			}
			result <- outputBuildLockResult{err: fmt.Errorf("acquire output build mutex: event=%d error=%v", event, waitErr)}
			return
		}
		result <- outputBuildLockResult{lock: lock}
		<-lock.release
		releaseErr := windows.ReleaseMutex(handle)
		closeErr := windows.CloseHandle(handle)
		lock.errMu.Lock()
		lock.err = errors.Join(releaseErr, closeErr)
		lock.errMu.Unlock()
		close(lock.done)
	}()
	acquired := <-result
	return acquired.lock, acquired.err
}

func (l *outputBuildLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() { close(l.release) })
	<-l.done
	l.errMu.Lock()
	defer l.errMu.Unlock()
	return l.err
}

func cleanupStaleBuildArtifacts(output string, _ time.Time) error {
	entries, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	var cleanupErrors []error
	for _, entry := range entries {
		name := entry.Name()
		isISODirectory := ownedISOWorkDirectory(name)
		isMySQLDirectory := ownedMySQLCacheDirectory(name)
		isDirectory := isISODirectory || isMySQLDirectory
		if !isDirectory && !ownedPartialFile(name) {
			continue
		}
		path := filepath.Join(output, name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if !os.IsNotExist(statErr) {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("inspect stale build artifact %s: %w", path, statErr))
			}
			continue
		}
		// The machine-wide package mutex is already held, so no live build can own these
		// exact Forge partial names in this directory. Reclaim them immediately;
		// a crashed multi-gigabyte payload must not block the next build for 24h.
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if isDirectory {
			owned := (isISODirectory && ownedISOWorkRoot(path)) || (isMySQLDirectory && ownedMySQLCacheRoot(path))
			if !info.IsDir() || !owned {
				continue
			}
			if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("remove stale owned build directory %s: %w", path, err))
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		remove := os.Remove
		if ownedPartialISO(name) {
			remove = iso.RemoveStaleBuildISO
		}
		if err := remove(path); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove stale partial build artifact %s: %w", path, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func writeActiveBuildMarker(output string) error {
	marker := activeBuildMarker{FormatVersion: 1, Product: "KiemTheDeployForge", OutputPath: filepath.Clean(output)}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := filepath.Join(output, activeBuildMarkerName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create active build marker: %w", err)
	}
	keep := true
	defer func() {
		_ = file.Close()
		if keep {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	keep = false
	return nil
}

func cleanupAbandonedBuildOutputs(output string) error {
	markerPath := filepath.Join(output, activeBuildMarkerName)
	markerInfo, err := os.Lstat(markerPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("active build marker is not a regular file: %s", markerPath)
	}
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return err
	}
	var marker activeBuildMarker
	if err := json.Unmarshal(raw, &marker); err != nil ||
		marker.FormatVersion != 1 ||
		marker.Product != "KiemTheDeployForge" ||
		!strings.EqualFold(filepath.Clean(marker.OutputPath), filepath.Clean(output)) {
		return fmt.Errorf("active build marker is invalid; preserving release files in %s", output)
	}
	for _, name := range []string{"Setup.exe", "Payload.ktpkg", "README.txt", "KiemTheServer-Offline.iso"} {
		path := filepath.Join(output, name)
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("abandoned build output is not a regular file; preserving %s", path)
		}
		remove := os.Remove
		if strings.EqualFold(name, "KiemTheServer-Offline.iso") {
			remove = iso.RemoveStaleBuildISO
		}
		if err := remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove abandoned build output %s: %w", path, err)
		}
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove active build marker: %w", err)
	}
	return nil
}

func removeActiveBuildMarker(output string) error {
	path := filepath.Join(output, activeBuildMarkerName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func cleanupStaleDownloadArtifacts(cacheRoot string, now time.Time) error {
	entries, err := os.ReadDir(cacheRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	prefix := "." + strings.ToLower(database.PinnedMySQLName) + "-"
	const suffix = ".download"
	var cleanupErrors []error
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) || len(name) <= len(prefix)+len(suffix) {
			continue
		}
		path := filepath.Join(cacheRoot, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if !os.IsNotExist(statErr) {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("inspect stale MySQL download %s: %w", path, statErr))
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || now.Sub(info.ModTime()) < staleDownloadArtifactAge {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove stale MySQL download %s: %w", path, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func ownedISOWorkDirectory(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, ".iso-build-") && len(lower) > len(".iso-build-")
}

func ownedISOWorkRoot(path string) bool {
	markerPath := filepath.Join(path, iso.WorkMarkerName)
	info, err := os.Lstat(markerPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != int64(len(iso.WorkMarkerContent)) {
		return false
	}
	raw, err := os.ReadFile(markerPath)
	return err == nil && string(raw) == iso.WorkMarkerContent
}

func ownedMySQLCacheDirectory(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, mysqlCachePrefix) && len(lower) > len(mysqlCachePrefix)
}

func ownedMySQLCacheRoot(path string) bool {
	markerPath := filepath.Join(path, mysqlCacheMarkerName)
	info, err := os.Lstat(markerPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != int64(len(mysqlCacheMarkerContent)) {
		return false
	}
	raw, err := os.ReadFile(markerPath)
	return err == nil && string(raw) == mysqlCacheMarkerContent
}

func ownedPartialFile(name string) bool {
	lower := strings.ToLower(name)
	patterns := [][2]string{
		{".setup.exe-", ".building"},
		{".payload.ktpkg-", ".building"},
		{".readme.txt-", ".building"},
		{".kiemtheserver-offline-", ".building.iso"},
	}
	for _, pattern := range patterns {
		if strings.HasPrefix(lower, pattern[0]) && strings.HasSuffix(lower, pattern[1]) && len(lower) > len(pattern[0])+len(pattern[1]) {
			return true
		}
	}
	return false
}

func ownedPartialISO(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, ".kiemtheserver-offline-") && strings.HasSuffix(lower, ".building.iso") && len(lower) > len(".kiemtheserver-offline-")+len(".building.iso")
}
