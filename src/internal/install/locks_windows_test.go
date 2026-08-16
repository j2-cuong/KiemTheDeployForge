package install

import (
	"strings"
	"testing"
)

func TestInstallLocksRejectConcurrentOwner(t *testing.T) {
	root := t.TempDir()
	first, err := acquireInstallLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := acquireInstallLocks(root)
	if second != nil {
		second.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("concurrent lock error = %v", err)
	}
}
