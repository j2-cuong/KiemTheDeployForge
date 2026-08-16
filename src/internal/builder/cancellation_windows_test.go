package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"kiemthedeployforge/internal/release"
)

func TestHashFileHonorsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := hashFile(ctx, path, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("hashFile error = %v, want context.Canceled", err)
	}
}

func TestEnumerateTreeHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, _, err := enumerateTree(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("enumerateTree error = %v, want context.Canceled", err)
	}
}

func TestWriteArchiveRemovesPartialOutputAfterCancellation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "payload.bin")
	if err := os.WriteFile(source, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "Setup.exe")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := writeArchive(ctx, output, []byte("MZ"), []byte("{}\n"), []sourceFile{{
		Source: source,
		Entry:  release.FileEntry{Path: "payload/file.bin", Size: int64(len("payload"))},
	}}, int64(len("payload")), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("writeArchive error = %v, want context.Canceled", err)
	}
	for _, path := range []string{output, output + ".building"} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("partial output remains at %s: %v", path, statErr)
		}
	}
}
