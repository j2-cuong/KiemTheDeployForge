package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ManifestRelativePath = "manifests/release.json"

// BotTargetRoot is the install directory the optional bot tree is written to.
// It lives here because the manifest validator has to recognise bot payload
// entries, and every other package can take the name from this one.
const BotTargetRoot = "Bot"

type Manifest struct {
	FormatVersion int              `json:"formatVersion"`
	Product       string           `json:"product"`
	ReleaseID     string           `json:"releaseId"`
	CreatedUTC    string           `json:"createdUtc"`
	PayloadBytes  int64            `json:"payloadBytes"`
	Directories   []DirectoryEntry `json:"directories,omitempty"`
	Files         []FileEntry      `json:"files"`
	MySQL         MySQLArtifact    `json:"mysql"`
	Database      SQLArtifact      `json:"database"`
	// Accounts are the MySQL credentials chosen when the release was built.
	Accounts Accounts `json:"accounts"`
	// IncludesBot records whether a bot directory was packaged. The bot is
	// optional, so Setup must not look for its files or rewrite its
	// configuration when the release was built without one.
	IncludesBot bool `json:"includesBot"`
}

type FileEntry struct {
	Path             string `json:"path"`
	Target           string `json:"target"`
	Size             int64  `json:"size"`
	SHA256           string `json:"sha256"`
	Attributes       uint32 `json:"attributes"`
	LastWriteTimeUTC string `json:"lastWriteTimeUtc"`
}

type DirectoryEntry struct {
	Target           string `json:"target"`
	Attributes       uint32 `json:"attributes"`
	LastWriteTimeUTC string `json:"lastWriteTimeUtc"`
}

type SQLArtifact struct {
	Path   string `json:"path"`
	Target string `json:"target"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type MySQLArtifact struct {
	Version string `json:"version"`
	Path    string `json:"path"`
	Target  string `json:"target"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	MD5     string `json:"md5"`
	Source  string `json:"source"`
}

func Parse(raw []byte) (*Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode release manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("release manifest contains trailing JSON data")
		}
		return nil, fmt.Errorf("decode trailing release manifest data: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func Load(mediaRoot, trustedSHA256 string, allowUnpinned bool) (*Manifest, string, error) {
	path, err := SafeJoin(mediaRoot, ManifestRelativePath)
	if err != nil {
		return nil, "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read release manifest: %w", err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	trusted := strings.ToLower(strings.TrimSpace(trustedSHA256))
	if trusted == "" || trusted == "unpinned" {
		if !allowUnpinned {
			return nil, digest, errors.New("Setup.exe is not pinned to this release manifest")
		}
	} else if trusted != digest {
		return nil, digest, fmt.Errorf("manifest digest mismatch: got %s", digest)
	}

	manifest, err := Parse(raw)
	if err != nil {
		return nil, digest, err
	}
	return manifest, digest, nil
}

func (m *Manifest) Validate() error {
	if m.FormatVersion != 1 {
		return fmt.Errorf("unsupported manifest format %d", m.FormatVersion)
	}
	if m.Product != "KiemTheDeployForge" {
		return fmt.Errorf("unexpected product %q", m.Product)
	}
	if strings.TrimSpace(m.ReleaseID) == "" {
		return errors.New("releaseId is required")
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedUTC); err != nil {
		return fmt.Errorf("invalid createdUtc: %w", err)
	}
	if len(m.Files) == 0 {
		return errors.New("manifest has no payload files")
	}
	seenSource := make(map[string]struct{}, len(m.Files))
	seenTarget := make(map[string]struct{}, len(m.Files)+len(m.Directories))
	for i, directory := range m.Directories {
		if err := validateRelativePath(directory.Target); err != nil {
			return fmt.Errorf("directories[%d].target: %w", i, err)
		}
		if _, err := time.Parse(time.RFC3339Nano, directory.LastWriteTimeUTC); err != nil {
			return fmt.Errorf("directories[%d].lastWriteTimeUtc: %w", i, err)
		}
		targetKey := strings.ToLower(filepath.Clean(filepath.FromSlash(directory.Target)))
		if _, exists := seenTarget[targetKey]; exists {
			return fmt.Errorf("duplicate install target %q", directory.Target)
		}
		seenTarget[targetKey] = struct{}{}
	}
	var total int64
	for i, file := range m.Files {
		if err := validateRelativePath(file.Path); err != nil {
			return fmt.Errorf("files[%d].path: %w", i, err)
		}
		if err := validateRelativePath(file.Target); err != nil {
			return fmt.Errorf("files[%d].target: %w", i, err)
		}
		if file.Size < 0 || !validSHA256(file.SHA256) {
			return fmt.Errorf("files[%d] has invalid size/hash", i)
		}
		if _, err := time.Parse(time.RFC3339Nano, file.LastWriteTimeUTC); err != nil {
			return fmt.Errorf("files[%d].lastWriteTimeUtc: %w", i, err)
		}
		sourceKey := strings.ToLower(filepath.Clean(filepath.FromSlash(file.Path)))
		targetKey := strings.ToLower(filepath.Clean(filepath.FromSlash(file.Target)))
		if _, exists := seenSource[sourceKey]; exists {
			return fmt.Errorf("duplicate media path %q", file.Path)
		}
		if _, exists := seenTarget[targetKey]; exists {
			return fmt.Errorf("duplicate install target %q", file.Target)
		}
		seenSource[sourceKey] = struct{}{}
		seenTarget[targetKey] = struct{}{}
		total += file.Size
	}
	if total != m.PayloadBytes {
		return fmt.Errorf("payload byte count mismatch: manifest=%d entries=%d", m.PayloadBytes, total)
	}
	if m.MySQL.Version != "5.5.15-win32" || m.MySQL.Size != 139896749 ||
		!validSHA256(m.MySQL.SHA256) || len(m.MySQL.MD5) != 32 {
		return errors.New("MySQL artifact is not the pinned 5.5.15 Win32 package")
	}
	if err := validateRelativePath(m.MySQL.Path); err != nil {
		return fmt.Errorf("mysql.path: %w", err)
	}
	if err := validateRelativePath(m.MySQL.Target); err != nil {
		return fmt.Errorf("mysql.target: %w", err)
	}
	var mysqlEntryFound bool
	for _, file := range m.Files {
		if strings.EqualFold(filepath.Clean(filepath.FromSlash(file.Path)), filepath.Clean(filepath.FromSlash(m.MySQL.Path))) &&
			strings.EqualFold(filepath.Clean(filepath.FromSlash(file.Target)), filepath.Clean(filepath.FromSlash(m.MySQL.Target))) &&
			file.Size == m.MySQL.Size && strings.EqualFold(file.SHA256, m.MySQL.SHA256) {
			mysqlEntryFound = true
			break
		}
	}
	if !mysqlEntryFound {
		return errors.New("pinned MySQL package is missing from payload files")
	}
	if err := validateRelativePath(m.Database.Path); err != nil {
		return fmt.Errorf("database.path: %w", err)
	}
	if err := validateRelativePath(m.Database.Target); err != nil {
		return fmt.Errorf("database.target: %w", err)
	}
	if m.Database.Size <= 0 || !validSHA256(m.Database.SHA256) {
		return errors.New("database artifact has invalid size/hash")
	}
	var databaseEntryFound bool
	for _, file := range m.Files {
		if strings.EqualFold(filepath.Clean(filepath.FromSlash(file.Path)), filepath.Clean(filepath.FromSlash(m.Database.Path))) &&
			strings.EqualFold(filepath.Clean(filepath.FromSlash(file.Target)), filepath.Clean(filepath.FromSlash(m.Database.Target))) &&
			file.Size == m.Database.Size && strings.EqualFold(file.SHA256, m.Database.SHA256) {
			databaseEntryFound = true
			break
		}
	}
	if !databaseEntryFound {
		return errors.New("database artifact is missing from payload files")
	}
	// A blank accounts block means the stock credentials, which is how every
	// consumer resolves it. Only values that were actually set can be wrong.
	if err := m.Accounts.WithDefaults().Validate(); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	// A release that claims a bot must actually carry one, and one that does
	// not must not smuggle bot files past Setup's configuration step.
	botPrefix := strings.ToLower(BotTargetRoot) + "/"
	var botFileFound bool
	for _, file := range m.Files {
		if strings.HasPrefix(strings.ToLower(filepath.ToSlash(file.Target)), botPrefix) {
			botFileFound = true
			break
		}
	}
	if m.IncludesBot != botFileFound {
		return fmt.Errorf("includesBot is %v but the payload %s bot files", m.IncludesBot, map[bool]string{true: "contains", false: "contains no"}[botFileFound])
	}
	return nil
}

func VerifyFile(path string, expectedSize int64, expectedSHA256 string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("size mismatch for %s: got %d want %d", path, info.Size(), expectedSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, expectedSHA256) {
		return fmt.Errorf("SHA-256 mismatch for %s", path)
	}
	return nil
}

func SafeJoin(root, relative string) (string, error) {
	if err := validateRelativePath(relative); err != nil {
		return "", err
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(cleanRoot, filepath.FromSlash(relative))
	rel, err := filepath.Rel(cleanRoot, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root: %q", relative)
	}
	return joined, nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("unsafe path %q", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe path %q", path)
	}
	if filepath.VolumeName(clean) != "" {
		return fmt.Errorf("path contains a volume name: %q", path)
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
