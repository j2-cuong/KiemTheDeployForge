package configpatch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPatchBytesPreservesEncodingAndEOL(t *testing.T) {
	original := append([]byte{0xef, 0xbb, 0xbf}, []byte("[Region_0]\r\nTitle=\x81\x82\x83\r\n1_Address = 192.168.1.10  \r\n[Other]\r\n1_Address=127.0.0.1\r\n")...)
	patched, matches, err := patchBytes(original, "Region_0", "1_Address", "192.168.50.83")
	if err != nil {
		t.Fatal(err)
	}
	if matches != 1 {
		t.Fatalf("matches=%d", matches)
	}
	if !bytes.Contains(patched, []byte("Title=\x81\x82\x83\r\n")) {
		t.Fatal("non-ASCII bytes or CRLF changed")
	}
	if !bytes.Contains(patched, []byte("1_Address = 192.168.50.83  \r\n")) {
		t.Fatal("target value was not patched exactly")
	}
	if !bytes.Contains(patched, []byte("[Other]\r\n1_Address=127.0.0.1\r\n")) {
		t.Fatal("another section was modified")
	}
}

func TestPatchBytesRejectsAmbiguousKey(t *testing.T) {
	input := []byte("[GameServer]\r\nInIp=1.1.1.1\r\nInIp=2.2.2.2\r\n")
	_, matches, err := patchBytes(input, "GameServer", "InIp", "192.168.1.8")
	if err != nil {
		t.Fatal(err)
	}
	if matches != 2 {
		t.Fatalf("matches=%d", matches)
	}
}

func TestPatchINIPreservesLastWriteTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.ini")
	if err := os.WriteFile(path, []byte("[GameServer]\r\nInIp=10.0.0.1\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wanted := time.Unix(1_600_000_000, 0).UTC()
	if err := os.Chtimes(path, wanted, wanted); err != nil {
		t.Fatal(err)
	}

	if err := PatchINI(path, "GameServer", "InIp", "192.168.50.10"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(wanted) {
		t.Fatalf("last-write time = %s, want %s", info.ModTime(), wanted)
	}
}
