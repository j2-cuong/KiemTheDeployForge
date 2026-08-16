package release

import (
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

	"kiemthedeployforge/internal/winfile"
)

type Progress struct {
	FileIndex   int
	FileCount   int
	CopiedBytes int64
	TotalBytes  int64
	Path        string
}

func CopyPayload(ctx context.Context, mediaRoot, stagingRoot string, manifest *Manifest, report func(Progress)) error {
	for _, directory := range sortedDirectories(manifest.Directories, false) {
		destination, err := SafeJoin(stagingRoot, directory.Target)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return err
		}
	}
	var copied int64
	for index, entry := range manifest.Files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		source, err := SafeJoin(mediaRoot, entry.Path)
		if err != nil {
			return err
		}
		destination, err := SafeJoin(stagingRoot, entry.Target)
		if err != nil {
			return err
		}
		if err := copyOne(ctx, source, destination, entry, func(delta int64) {
			copied += delta
			if report != nil {
				report(Progress{index + 1, len(manifest.Files), copied, manifest.PayloadBytes, entry.Target})
			}
		}); err != nil {
			return err
		}
	}
	for _, directory := range sortedDirectories(manifest.Directories, true) {
		destination, err := SafeJoin(stagingRoot, directory.Target)
		if err != nil {
			return err
		}
		stamp, err := time.Parse(time.RFC3339Nano, directory.LastWriteTimeUTC)
		if err != nil {
			return err
		}
		if err := os.Chtimes(destination, stamp, stamp); err != nil {
			return err
		}
		if err := winfile.SetAttributes(destination, directory.Attributes); err != nil {
			return err
		}
	}
	return nil
}

func sortedDirectories(directories []DirectoryEntry, deepestFirst bool) []DirectoryEntry {
	ordered := append([]DirectoryEntry(nil), directories...)
	sort.Slice(ordered, func(i, j int) bool {
		leftDepth := strings.Count(strings.Trim(filepath.ToSlash(ordered[i].Target), "/"), "/")
		rightDepth := strings.Count(strings.Trim(filepath.ToSlash(ordered[j].Target), "/"), "/")
		if leftDepth != rightDepth {
			if deepestFirst {
				return leftDepth > rightDepth
			}
			return leftDepth < rightDepth
		}
		if deepestFirst {
			return strings.ToLower(ordered[i].Target) > strings.ToLower(ordered[j].Target)
		}
		return strings.ToLower(ordered[i].Target) < strings.ToLower(ordered[j].Target)
	})
	return ordered
}

func copyOne(ctx context.Context, source, destination string, entry FileEntry, onBytes func(int64)) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", source, err)
	}
	if !info.Mode().IsRegular() || info.Size() != entry.Size {
		return fmt.Errorf("invalid source file %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temp := destination + fmt.Sprintf(".partial-%d", os.Getpid())
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	keepTemp := true
	defer func() {
		output.Close()
		if keepTemp {
			_ = os.Remove(temp)
		}
	}()

	hash := sha256.New()
	buffer := make([]byte, 4*1024*1024)
	var written int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		read, readErr := input.Read(buffer)
		if read > 0 {
			if _, err := output.Write(buffer[:read]); err != nil {
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
			return readErr
		}
	}
	if written != entry.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), entry.SHA256) {
		return fmt.Errorf("payload verification failed while copying %s", entry.Path)
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	stamp, err := time.Parse(time.RFC3339Nano, entry.LastWriteTimeUTC)
	if err != nil {
		return err
	}
	if err := os.Chtimes(temp, stamp, stamp); err != nil {
		return err
	}
	if err := winfile.SetAttributes(temp, entry.Attributes); err != nil {
		return err
	}
	if err := os.Rename(temp, destination); err != nil {
		return err
	}
	keepTemp = false
	return nil
}
