package install

import (
	"errors"
	"testing"
)

func TestInstallMediaCloseRetainsMountOwnershipForRetry(t *testing.T) {
	dismountErr := errors.New("virtual disk is busy")
	attempts := 0
	media := &installMedia{
		isoPath: `D:\release\KiemTheServer-Offline.iso`,
		mounted: true,
		dismount: func(string) error {
			attempts++
			if attempts == 1 {
				return dismountErr
			}
			return nil
		},
	}

	if err := media.Close(); !errors.Is(err, dismountErr) {
		t.Fatalf("first Close error = %v, want %v", err, dismountErr)
	}
	if !media.mounted {
		t.Fatal("failed dismount discarded mount ownership")
	}
	if err := media.Close(); err != nil {
		t.Fatalf("retry Close failed: %v", err)
	}
	if media.mounted || attempts != 2 {
		t.Fatalf("retry state: mounted=%v attempts=%d", media.mounted, attempts)
	}
}

func TestJoinMediaCloseErrorPreservesOperationAndCleanupFailures(t *testing.T) {
	operationErr := errors.New("plan failed")
	closeErr := errors.New("dismount failed")
	joined := joinMediaCloseError(operationErr, closeErr)
	if !errors.Is(joined, operationErr) || !errors.Is(joined, closeErr) {
		t.Fatalf("joined error lost a cause: %v", joined)
	}
}
