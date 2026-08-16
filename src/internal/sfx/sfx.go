package sfx

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kiemthedeployforge/internal/release"
	"kiemthedeployforge/internal/winfile"
)

const ManifestPath = "manifests/release.json"
const PayloadFileName = "Payload.ktpkg"

type Package struct {
	file           *os.File
	reader         *zip.Reader
	entries        map[string]*zip.File
	Manifest       *release.Manifest
	ManifestSHA256 string
}

type Progress struct {
	FileIndex   int
	FileCount   int
	CopiedBytes int64
	TotalBytes  int64
	Path        string
}

type RepairMode uint8

const (
	RepairSkip RepairMode = iota
	RepairMissing
	RepairVerify
)

func Open(path string) (*Package, error) {
	return open(path, true)
}

// OpenManifest reads the release pin embedded in the small Setup.exe bootstrap.
// Payload entries are intentionally stored in Payload.ktpkg instead of the PE.
func OpenManifest(path string) (*Package, error) {
	return open(path, false)
}

func open(path string, requirePayload bool) (*Package, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("open ZIP64 package: %w", err)
	}
	entries := make(map[string]*zip.File, len(reader.File))
	for _, entry := range reader.File {
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "../") {
			file.Close()
			return nil, fmt.Errorf("unsafe archive path %q", entry.Name)
		}
		key := strings.ToLower(name)
		if _, exists := entries[key]; exists {
			file.Close()
			return nil, fmt.Errorf("duplicate archive path %q", entry.Name)
		}
		entries[key] = entry
	}
	manifestEntry := entries[strings.ToLower(ManifestPath)]
	if manifestEntry == nil || manifestEntry.UncompressedSize64 > 64*1024*1024 {
		file.Close()
		return nil, fmt.Errorf("release manifest is missing or too large")
	}
	manifestReader, err := manifestEntry.Open()
	if err != nil {
		file.Close()
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(manifestReader, 64*1024*1024+1))
	manifestReader.Close()
	if err != nil {
		file.Close()
		return nil, err
	}
	manifest, err := release.Parse(raw)
	if err != nil {
		file.Close()
		return nil, err
	}
	sum := sha256.Sum256(raw)
	pack := &Package{file: file, reader: reader, entries: entries, Manifest: manifest, ManifestSHA256: hex.EncodeToString(sum[:])}
	if requirePayload {
		if err := pack.validateEntries(); err != nil {
			file.Close()
			return nil, err
		}
	}
	return pack, nil
}

func (p *Package) Close() error {
	if p == nil || p.file == nil {
		return nil
	}
	return p.file.Close()
}

func (p *Package) VerifyAll(ctx context.Context, report func(Progress)) error {
	var copied int64
	buffer := make([]byte, 4*1024*1024)
	for index, manifestEntry := range p.Manifest.Files {
		entry := p.entries[strings.ToLower(strings.ReplaceAll(manifestEntry.Path, "\\", "/"))]
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		hash := sha256.New()
		var readTotal int64
		for {
			select {
			case <-ctx.Done():
				reader.Close()
				return ctx.Err()
			default:
			}
			read, readErr := reader.Read(buffer)
			if read > 0 {
				_, _ = hash.Write(buffer[:read])
				readTotal += int64(read)
				copied += int64(read)
				if report != nil {
					report(Progress{index + 1, len(p.Manifest.Files), copied, p.Manifest.PayloadBytes, manifestEntry.Path})
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				reader.Close()
				return readErr
			}
		}
		reader.Close()
		if readTotal != manifestEntry.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), manifestEntry.SHA256) {
			return fmt.Errorf("offline payload hash mismatch: %s", manifestEntry.Path)
		}
	}
	return nil
}

func (p *Package) Extract(ctx context.Context, stagingRoot string, report func(Progress)) error {
	if err := createDirectories(stagingRoot, p.Manifest.Directories); err != nil {
		return err
	}
	var copied int64
	buffer := make([]byte, 4*1024*1024)
	for index, manifestEntry := range p.Manifest.Files {
		entry := p.entries[strings.ToLower(strings.ReplaceAll(manifestEntry.Path, "\\", "/"))]
		destination, err := release.SafeJoin(stagingRoot, manifestEntry.Target)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		temp := destination + fmt.Sprintf(".extract-%d", os.Getpid())
		input, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			input.Close()
			return err
		}
		hash := sha256.New()
		var written int64
		copyErr := error(nil)
		for {
			select {
			case <-ctx.Done():
				copyErr = ctx.Err()
			default:
			}
			if copyErr != nil {
				break
			}
			read, readErr := input.Read(buffer)
			if read > 0 {
				if _, err := output.Write(buffer[:read]); err != nil {
					copyErr = err
					break
				}
				_, _ = hash.Write(buffer[:read])
				written += int64(read)
				copied += int64(read)
				if report != nil {
					report(Progress{index + 1, len(p.Manifest.Files), copied, p.Manifest.PayloadBytes, manifestEntry.Target})
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				copyErr = readErr
				break
			}
		}
		input.Close()
		if syncErr := output.Sync(); copyErr == nil {
			copyErr = syncErr
		}
		if closeErr := output.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil || written != manifestEntry.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), manifestEntry.SHA256) {
			_ = os.Remove(temp)
			if copyErr != nil {
				return copyErr
			}
			return fmt.Errorf("offline payload verification failed: %s", manifestEntry.Path)
		}
		stamp, err := time.Parse(time.RFC3339Nano, manifestEntry.LastWriteTimeUTC)
		if err != nil {
			_ = os.Remove(temp)
			return err
		}
		if err := os.Chtimes(temp, stamp, stamp); err != nil {
			_ = os.Remove(temp)
			return err
		}
		if err := winfile.SetAttributes(temp, manifestEntry.Attributes); err != nil {
			_ = os.Remove(temp)
			return err
		}
		if err := os.Rename(temp, destination); err != nil {
			_ = os.Remove(temp)
			return err
		}
	}
	return applyDirectoryMetadata(stagingRoot, p.Manifest.Directories)
}

func (p *Package) Repair(ctx context.Context, installRoot string, decide func(release.FileEntry) RepairMode, report func(Progress)) (int, error) {
	if decide == nil {
		return 0, nil
	}
	modes := make([]RepairMode, len(p.Manifest.Files))
	managedDirectories := make(map[string]struct{})
	for index, entry := range p.Manifest.Files {
		modes[index] = decide(entry)
		if modes[index] == RepairSkip {
			continue
		}
		for parent := filepath.Dir(filepath.FromSlash(entry.Target)); parent != "."; parent = filepath.Dir(parent) {
			managedDirectories[strings.ToLower(filepath.Clean(parent))] = struct{}{}
		}
	}
	for _, directory := range p.Manifest.Directories {
		key := strings.ToLower(filepath.Clean(filepath.FromSlash(directory.Target)))
		if _, managed := managedDirectories[key]; !managed {
			continue
		}
		path, err := release.SafeJoin(installRoot, directory.Target)
		if err != nil {
			return 0, err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return 0, err
		}
	}
	buffer := make([]byte, 4*1024*1024)
	repaired := 0
	var copied int64
	for index, manifestEntry := range p.Manifest.Files {
		mode := modes[index]
		if mode == RepairSkip {
			continue
		}
		select {
		case <-ctx.Done():
			return repaired, ctx.Err()
		default:
		}
		destination, err := release.SafeJoin(installRoot, manifestEntry.Target)
		if err != nil {
			return repaired, err
		}
		info, statErr := os.Lstat(destination)
		needsRepair := os.IsNotExist(statErr)
		if statErr != nil && !os.IsNotExist(statErr) {
			return repaired, statErr
		}
		if statErr == nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return repaired, fmt.Errorf("refusing to replace non-regular installed payload: %s", manifestEntry.Target)
			}
			if mode == RepairVerify {
				needsRepair = release.VerifyFile(destination, manifestEntry.Size, manifestEntry.SHA256) != nil
			}
		}
		if !needsRepair {
			continue
		}
		entry := p.entries[strings.ToLower(strings.ReplaceAll(manifestEntry.Path, "\\", "/"))]
		if entry == nil {
			return repaired, fmt.Errorf("offline payload is missing %s", manifestEntry.Path)
		}
		if err := repairEntry(ctx, entry, destination, manifestEntry, buffer, func(delta int64) {
			copied += delta
			if report != nil {
				report(Progress{index + 1, len(p.Manifest.Files), copied, p.Manifest.PayloadBytes, manifestEntry.Target})
			}
		}); err != nil {
			return repaired, err
		}
		repaired++
	}
	return repaired, nil
}

func repairEntry(ctx context.Context, entry *zip.File, destination string, manifestEntry release.FileEntry, buffer []byte, onBytes func(int64)) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+"-*.repair")
	if err != nil {
		return err
	}
	temp := output.Name()
	keep := true
	defer func() {
		_ = output.Close()
		if keep {
			_ = os.Remove(temp)
		}
	}()
	input, err := entry.Open()
	if err != nil {
		return err
	}
	hash := sha256.New()
	var written int64
	for {
		select {
		case <-ctx.Done():
			input.Close()
			return ctx.Err()
		default:
		}
		read, readErr := input.Read(buffer)
		if read > 0 {
			if _, err := output.Write(buffer[:read]); err != nil {
				input.Close()
				return err
			}
			_, _ = hash.Write(buffer[:read])
			written += int64(read)
			onBytes(int64(read))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			input.Close()
			return readErr
		}
	}
	if err := input.Close(); err != nil {
		return err
	}
	if written != manifestEntry.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), manifestEntry.SHA256) {
		return fmt.Errorf("offline payload verification failed while repairing %s", manifestEntry.Path)
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	stamp, err := time.Parse(time.RFC3339Nano, manifestEntry.LastWriteTimeUTC)
	if err != nil {
		return err
	}
	if err := os.Chtimes(temp, stamp, stamp); err != nil {
		return err
	}
	if err := winfile.SetAttributes(temp, manifestEntry.Attributes); err != nil {
		return err
	}
	if oldAttributes, err := winfile.Attributes(destination); err == nil {
		const readOnly = uint32(0x1)
		if oldAttributes&readOnly != 0 {
			if err := winfile.SetAttributes(destination, oldAttributes&^readOnly); err != nil {
				return err
			}
		}
		if err := winfile.Replace(temp, destination); err != nil {
			_ = winfile.SetAttributes(destination, oldAttributes)
			return err
		}
	} else if os.IsNotExist(err) {
		if err := os.Rename(temp, destination); err != nil {
			return err
		}
	} else {
		return err
	}
	keep = false
	return nil
}

func createDirectories(stagingRoot string, directories []release.DirectoryEntry) error {
	ordered := append([]release.DirectoryEntry(nil), directories...)
	sort.Slice(ordered, func(i, j int) bool {
		leftDepth := pathDepth(ordered[i].Target)
		rightDepth := pathDepth(ordered[j].Target)
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return strings.ToLower(ordered[i].Target) < strings.ToLower(ordered[j].Target)
	})
	for _, directory := range ordered {
		path, err := release.SafeJoin(stagingRoot, directory.Target)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func applyDirectoryMetadata(stagingRoot string, directories []release.DirectoryEntry) error {
	ordered := append([]release.DirectoryEntry(nil), directories...)
	sort.Slice(ordered, func(i, j int) bool {
		leftDepth := pathDepth(ordered[i].Target)
		rightDepth := pathDepth(ordered[j].Target)
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return strings.ToLower(ordered[i].Target) > strings.ToLower(ordered[j].Target)
	})
	for _, directory := range ordered {
		path, err := release.SafeJoin(stagingRoot, directory.Target)
		if err != nil {
			return err
		}
		stamp, err := time.Parse(time.RFC3339Nano, directory.LastWriteTimeUTC)
		if err != nil {
			return err
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			return err
		}
		if err := winfile.SetAttributes(path, directory.Attributes); err != nil {
			return err
		}
	}
	return nil
}

func pathDepth(path string) int {
	clean := strings.Trim(filepath.ToSlash(path), "/")
	if clean == "" {
		return 0
	}
	return strings.Count(clean, "/") + 1
}

func (p *Package) validateEntries() error {
	expectedEntries := make(map[string]struct{}, len(p.Manifest.Files)+1)
	expectedEntries[strings.ToLower(ManifestPath)] = struct{}{}
	for _, manifestEntry := range p.Manifest.Files {
		key := strings.ToLower(strings.ReplaceAll(manifestEntry.Path, "\\", "/"))
		expectedEntries[key] = struct{}{}
		entry := p.entries[key]
		if entry == nil {
			return fmt.Errorf("offline payload is missing %s", manifestEntry.Path)
		}
		if entry.FileInfo().IsDir() {
			return fmt.Errorf("payload file is encoded as a directory: %s", manifestEntry.Path)
		}
		if entry.Method != zip.Store {
			return fmt.Errorf("payload must use ZIP Store method: %s", manifestEntry.Path)
		}
		if int64(entry.UncompressedSize64) != manifestEntry.Size {
			return fmt.Errorf("offline payload size mismatch: %s", manifestEntry.Path)
		}
	}
	for key, entry := range p.entries {
		if _, expected := expectedEntries[key]; !expected {
			return fmt.Errorf("offline payload contains unmanifested entry %s", entry.Name)
		}
	}
	return nil
}
