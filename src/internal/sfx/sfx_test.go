package sfx

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiemthedeployforge/internal/release"
)

func TestOpenManifestAcceptsSmallBootstrapWithoutPayloadEntries(t *testing.T) {
	manifest := `{
  "formatVersion": 1,
  "product": "KiemTheDeployForge",
  "releaseId": "test-release",
  "createdUtc": "2026-08-10T00:00:00Z",
  "payloadBytes": 139896750,
  "files": [
    {"path":"prerequisites/mysql.zip","target":"InstallerData/packages/mysql.zip","size":139896749,"sha256":"` + strings.Repeat("a", 64) + `","attributes":0,"lastWriteTimeUtc":"2026-08-10T00:00:00Z"},
    {"path":"database/jxaccount.sql","target":"InstallerData/database/jxaccount.sql","size":1,"sha256":"` + strings.Repeat("b", 64) + `","attributes":0,"lastWriteTimeUtc":"2026-08-10T00:00:00Z"}
  ],
  "mysql":{"version":"5.5.15-win32","path":"prerequisites/mysql.zip","target":"InstallerData/packages/mysql.zip","size":139896749,"sha256":"` + strings.Repeat("a", 64) + `","md5":"` + strings.Repeat("c", 32) + `","source":"offline"},
  "database":{"path":"database/jxaccount.sql","target":"InstallerData/database/jxaccount.sql","size":1,"sha256":"` + strings.Repeat("b", 64) + `"}
}`
	var buffer bytes.Buffer
	buffer.WriteString("MZ bootstrap")
	writer := zip.NewWriter(&buffer)
	writer.SetOffset(int64(len("MZ bootstrap")))
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: ManifestPath, Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "Setup.exe")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := OpenManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.Close()
	if payload, err := Open(path); err == nil {
		payload.Close()
		t.Fatal("full payload open unexpectedly accepted a manifest-only bootstrap")
	}
}

func TestDirectoryMetadataPreservesEmptyDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "staging")
	stamp := time.Date(2020, time.July, 8, 9, 10, 12, 0, time.UTC)
	directories := []release.DirectoryEntry{
		{Target: "Client/empty/deep", Attributes: 0x10, LastWriteTimeUTC: stamp.Format(time.RFC3339Nano)},
		{Target: "Client", Attributes: 0x10, LastWriteTimeUTC: stamp.Format(time.RFC3339Nano)},
		{Target: "Client/empty", Attributes: 0x10, LastWriteTimeUTC: stamp.Format(time.RFC3339Nano)},
	}
	if err := createDirectories(root, directories); err != nil {
		t.Fatal(err)
	}
	if err := applyDirectoryMetadata(root, directories); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "Client", "empty", "deep")
	info, err := os.Stat(deep)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("expected an empty directory at %s", deep)
	}
	if difference := info.ModTime().UTC().Sub(stamp); difference < -2*time.Second || difference > 2*time.Second {
		t.Fatalf("directory timestamp = %s, want %s", info.ModTime().UTC(), stamp)
	}
}

func TestRepairRestoresMissingAndCorruptPayload(t *testing.T) {
	payload := []byte("verified payload")
	sum := sha256.Sum256(payload)
	stamp := time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339Nano)
	archivePath := filepath.Join(t.TempDir(), "payload.zip")
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archiveFile)
	entryWriter, err := writer.CreateHeader(&zip.FileHeader{Name: "payload/Client/Game.exe", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entryWriter.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := opened.Stat()
	reader, err := zip.NewReader(opened, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	manifestEntry := release.FileEntry{Path: "payload/Client/Game.exe", Target: "Client/Game.exe", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:]), LastWriteTimeUTC: stamp}
	pack := &Package{file: opened, reader: reader, entries: map[string]*zip.File{"payload/client/game.exe": reader.File[0]}, Manifest: &release.Manifest{Files: []release.FileEntry{manifestEntry}, PayloadBytes: int64(len(payload))}}
	defer pack.Close()
	root := t.TempDir()
	destination := filepath.Join(root, "Client", "Game.exe")
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	repaired, err := pack.Repair(context.Background(), root, func(release.FileEntry) RepairMode { return RepairVerify }, nil)
	if err != nil || repaired != 1 {
		t.Fatalf("repair result: repaired=%d error=%v", repaired, err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("repaired payload = %q error=%v", got, err)
	}
}
