package install

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Journal struct {
	mu          sync.Mutex
	file        *os.File
	OperationID string
	Path        string
}

type journalEntry struct {
	TimeUTC      string `json:"timeUtc"`
	OperationID  string `json:"operationId"`
	Phase        string `json:"phase"`
	Resource     string `json:"resource,omitempty"`
	Action       string `json:"action"`
	Status       string `json:"status"`
	Owned        bool   `json:"ownedByInstaller"`
	ErrorMessage string `json:"error,omitempty"`
}

func NewJournal(operationID string) (*Journal, error) {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = filepath.Join(os.TempDir(), "ProgramData")
	}
	dir := filepath.Join(base, "KiemTheDeployForge", "InstallerLogs", operationID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "journal.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Journal{file: file, OperationID: operationID, Path: path}, nil
}

func (j *Journal) Record(phase, resource, action, status string, owned bool, operationErr error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry := journalEntry{
		TimeUTC: time.Now().UTC().Format(time.RFC3339Nano), OperationID: j.OperationID,
		Phase: phase, Resource: resource, Action: action, Status: status, Owned: owned,
	}
	if operationErr != nil {
		entry.ErrorMessage = operationErr.Error()
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(j.file, "%s\n", raw); err != nil {
		return err
	}
	return j.file.Sync()
}

func (j *Journal) Close() error {
	if j == nil || j.file == nil {
		return nil
	}
	return j.file.Close()
}
