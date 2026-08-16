package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadPinnedFileRejectsContentLengthMismatch(t *testing.T) {
	payload := []byte("pinned payload")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "15")
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "payload.bin")
	err := downloadPinnedFile(context.Background(), server.Client(), server.URL, destination, int64(len(payload)), digest(payload), nil)
	if err == nil || !strings.Contains(err.Error(), "Content-Length mismatch") {
		t.Fatalf("expected Content-Length mismatch, got %v", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("destination should not be published, stat error: %v", statErr)
	}
}

func TestDownloadPinnedFileStopsOnOversizeChunkedBody(t *testing.T) {
	payload := []byte("12345678")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		flusher := writer.(http.Flusher)
		_, _ = writer.Write(payload)
		flusher.Flush()
		_, _ = writer.Write([]byte("x"))
	}))
	defer server.Close()

	directory := t.TempDir()
	destination := filepath.Join(directory, "payload.bin")
	err := downloadPinnedFile(context.Background(), server.Client(), server.URL, destination, int64(len(payload)), digest(payload), nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded pinned size") {
		t.Fatalf("expected oversize rejection, got %v", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("destination should not be published, stat error: %v", statErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(directory, ".*.download"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary downloads were not removed: %v", matches)
	}
}

func TestDownloadPinnedFilePublishesVerifiedSHA256(t *testing.T) {
	payload := []byte("the SHA-256 digest is authoritative")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "payload.bin")
	if err := downloadPinnedFile(context.Background(), server.Client(), server.URL, destination, int64(len(payload)), digest(payload), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("published payload = %q, want %q", got, payload)
	}
}

func TestEnumerateTreeIncludesEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty", "deep")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(root, "payload.bin")
	if err := os.WriteFile(filePath, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, directories, total, err := enumerateTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != filePath || total != int64(len("payload")) {
		t.Fatalf("unexpected file inventory: files=%v total=%d", files, total)
	}
	wanted := map[string]bool{root: true, filepath.Join(root, "empty"): true, empty: true}
	for _, directory := range directories {
		delete(wanted, directory)
	}
	if len(wanted) != 0 {
		t.Fatalf("missing directories: %v", wanted)
	}
}

func TestBuildDiskRequirementKeepsTwoPayloadCopiesAndHeadroom(t *testing.T) {
	const payload = int64(10 * 1024 * 1024 * 1024)
	got, err := requiredBuildBytes(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(payload)*2 + uint64(payload)/5 + buildDiskHeadroom
	if got != want {
		t.Fatalf("requiredBuildBytes(%d) = %d, want %d", payload, got, want)
	}
}

func TestISODiskRequirementUsesActualPackageSize(t *testing.T) {
	const payloadPackage = int64(9 * 1024 * 1024 * 1024)
	got, err := requiredISOBytes(payloadPackage)
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(payloadPackage) + uint64(payloadPackage)/10 + buildDiskHeadroom
	if got != want {
		t.Fatalf("requiredISOBytes(%d) = %d, want %d", payloadPackage, got, want)
	}
}

func TestDiskRequirementRejectsOverflow(t *testing.T) {
	if _, err := addDiskBytes(math.MaxUint64, 1); err == nil {
		t.Fatal("expected disk requirement overflow")
	}
	if _, err := addPayloadBytes(math.MaxInt64, 1); err == nil {
		t.Fatal("expected payload byte count overflow")
	}
}

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
