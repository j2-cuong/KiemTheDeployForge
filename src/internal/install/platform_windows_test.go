package install

import (
	"strings"
	"testing"
)

func TestRequireFixedNTFSAcceptsTestVolume(t *testing.T) {
	info, err := RequireFixedNTFS(t.TempDir(), "test directory")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(info.FileSystem, "NTFS") || info.DriveType != 3 {
		t.Fatalf("unexpected volume info: %+v", info)
	}
}
