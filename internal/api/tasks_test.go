package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cryp/internal/pathguard"
	"cryp/internal/storage"
)

func TestTaskResponseRedactsHostPathAndDiagnostics(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "imports", "videos")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatalf("create source: %v", err)
	}
	guard, err := pathguard.New(root)
	if err != nil {
		t.Fatalf("create source guard: %v", err)
	}
	s := &Server{sourceGuard: guard}
	record := &storage.TaskRecord{
		ID:         "task",
		VaultID:    "vault",
		Type:       "import",
		Status:     "error",
		SourcePath: source,
		DestPath:   "/videos",
		ErrorMsg:   "read /secret/host/path: permission denied",
	}

	response := s.toTaskResponse(record)
	if response.SourcePath != "imports/videos" {
		t.Fatalf("sourcePath = %q, want root-relative path", response.SourcePath)
	}
	if response.ErrorMsg == record.ErrorMsg || strings.Contains(response.ErrorMsg, "permission denied") || strings.Contains(response.ErrorMsg, "/secret") {
		t.Fatalf("task diagnostics were not redacted: %q", response.ErrorMsg)
	}
	if response.DestPath != record.DestPath {
		t.Fatalf("destPath = %q, want %q", response.DestPath, record.DestPath)
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal task response: %v", err)
	}
	if strings.Contains(string(encoded), root) || strings.Contains(string(encoded), "permission denied") {
		t.Fatalf("serialized task response leaked internal data: %s", encoded)
	}
}

func TestTaskResponseFallsBackToBasenameOutsideSourceRoot(t *testing.T) {
	s := &Server{}
	response := s.toTaskResponse(&storage.TaskRecord{SourcePath: "/outside/private/archive"})
	if response.SourcePath != "archive" {
		t.Fatalf("outside sourcePath = %q, want basename fallback", response.SourcePath)
	}
}
