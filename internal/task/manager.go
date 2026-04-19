package task

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cryp/internal/crypto"
	"cryp/internal/storage"
	"cryp/internal/thumbnail"
)

// ThumbEnqueuer is called after each file is encrypted to generate thumbnails
type ThumbEnqueuer interface {
	Enqueue(vaultID, vaultPath string, keys *crypto.VaultKeys, virtualPath string)
}

// Manager handles background task execution
type Manager struct {
	db      *storage.DB
	mu      sync.RWMutex
	running map[string]chan struct{} // taskID -> cancel channel
	thumbs  ThumbEnqueuer
}

const errTaskCancelled = "task cancelled"

// NewManager creates a new task manager and recovers interrupted tasks
func NewManager(db *storage.DB) *Manager {
	m := &Manager{
		db:      db,
		running: make(map[string]chan struct{}),
	}
	// Mark previously running tasks as interrupted
	m.recoverTasks()
	return m
}

// SetThumbEnqueuer sets the thumbnail enqueuer (avoids circular init)
func (m *Manager) SetThumbEnqueuer(t ThumbEnqueuer) {
	m.thumbs = t
}

// recoverTasks marks any tasks that were "running" when server restarted as "error"
func (m *Manager) recoverTasks() {
	tasks, err := m.db.ListRunningTasks()
	if err != nil {
		log.Printf("Failed to recover tasks: %v", err)
		return
	}
	for _, t := range tasks {
		t.Status = "error"
		t.ErrorMsg = "server restarted, task interrupted"
		_ = m.db.UpdateTask(&t)
	}
}

// StartImport starts a background directory import+encrypt task
func (m *Manager) StartImport(taskID, vaultID, vaultPath string, keys *crypto.VaultKeys, sourcePath, destPath string, deleteSource bool) error {
	// Count files first
	totalFiles, totalBytes, err := countFiles(sourcePath)
	if err != nil {
		return fmt.Errorf("count files: %w", err)
	}

	t := &storage.TaskRecord{
		ID:           taskID,
		VaultID:      vaultID,
		Type:         "import",
		Status:       "running",
		TotalFiles:   totalFiles,
		TotalBytes:   totalBytes,
		SourcePath:   sourcePath,
		DestPath:     destPath,
		DeleteSource: deleteSource,
		StartedAt:    time.Now().Unix(),
	}

	if err := m.db.CreateTask(t); err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	cancel := make(chan struct{})
	m.mu.Lock()
	m.running[taskID] = cancel
	m.mu.Unlock()

	go m.runImport(t, vaultPath, keys, cancel)
	return nil
}

// StartRebuildIndex starts a background task that rebuilds the vault file index.
func (m *Manager) StartRebuildIndex(taskID, vaultID, vaultPath string, keys *crypto.VaultKeys) error {
	vault := &crypto.Vault{
		ID:   vaultID,
		Path: vaultPath,
		Keys: keys,
	}

	totalFiles, totalBytes, err := countVaultFiles(vault)
	if err != nil {
		return fmt.Errorf("count vault files: %w", err)
	}

	t := &storage.TaskRecord{
		ID:         taskID,
		VaultID:    vaultID,
		Type:       "index",
		Status:     "running",
		TotalFiles: totalFiles,
		TotalBytes: totalBytes,
		StartedAt:  time.Now().Unix(),
	}

	if err := m.db.CreateTask(t); err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	cancel := make(chan struct{})
	m.mu.Lock()
	m.running[taskID] = cancel
	m.mu.Unlock()

	go m.runRebuildIndex(t, vault, cancel)
	return nil
}

// CreateUploadTask creates a task record for tracking uploads
func (m *Manager) CreateUploadTask(taskID, vaultID string, totalFiles int, totalBytes int64) error {
	t := &storage.TaskRecord{
		ID:         taskID,
		VaultID:    vaultID,
		Type:       "upload",
		Status:     "running",
		TotalFiles: totalFiles,
		TotalBytes: totalBytes,
		StartedAt:  time.Now().Unix(),
	}
	return m.db.CreateTask(t)
}

// UpdateUploadProgress updates the progress of an upload task
func (m *Manager) UpdateUploadProgress(taskID string, processedFiles int, processedBytes int64, currentFile string) {
	t, err := m.db.GetTask(taskID)
	if err != nil {
		return
	}
	t.ProcessedFiles = processedFiles
	t.ProcessedBytes = processedBytes
	t.CurrentFile = currentFile
	if processedFiles >= t.TotalFiles {
		t.Status = "done"
	}
	_ = m.db.UpdateTask(t)
}

// CompleteUploadTask marks an upload task as done
func (m *Manager) CompleteUploadTask(taskID string) {
	t, err := m.db.GetTask(taskID)
	if err != nil {
		return
	}
	t.Status = "done"
	t.ProcessedFiles = t.TotalFiles
	t.ProcessedBytes = t.TotalBytes
	_ = m.db.UpdateTask(t)
}

// FailUploadTask marks an upload task as error
func (m *Manager) FailUploadTask(taskID, errMsg string) {
	t, err := m.db.GetTask(taskID)
	if err != nil {
		return
	}
	t.Status = "error"
	t.ErrorMsg = errMsg
	_ = m.db.UpdateTask(t)
}

// CancelTask cancels a running task
func (m *Manager) CancelTask(taskID string) bool {
	m.mu.Lock()
	cancel, ok := m.running[taskID]
	if ok {
		close(cancel)
		delete(m.running, taskID)
	}
	m.mu.Unlock()

	if ok {
		t, err := m.db.GetTask(taskID)
		if err == nil {
			t.Status = "cancelled"
			_ = m.db.UpdateTask(t)
		}
	}
	return ok
}

func (m *Manager) runImport(t *storage.TaskRecord, vaultPath string, keys *crypto.VaultKeys, cancel chan struct{}) {
	defer func() {
		m.mu.Lock()
		delete(m.running, t.ID)
		m.mu.Unlock()
	}()

	vault := &crypto.Vault{
		ID:   t.VaultID,
		Path: vaultPath,
		Keys: keys,
	}

	err := m.encryptDirRecursive(vault, keys, t, t.SourcePath, t.DestPath, cancel)
	if err != nil {
		if err.Error() == errTaskCancelled {
			t.Status = "cancelled"
			t.ErrorMsg = ""
		} else {
			t.Status = "error"
			t.ErrorMsg = err.Error()
		}
	} else {
		t.Status = "done"
	}

	// Clean up empty directories left after per-file deletion
	if t.DeleteSource {
		removeEmptyDirs(t.SourcePath)
	}

	_ = m.db.UpdateTask(t)

	crypto.ReleaseMemoryAfterLargeFile()
}

func (m *Manager) runRebuildIndex(t *storage.TaskRecord, vault *crypto.Vault, cancel chan struct{}) {
	defer func() {
		m.mu.Lock()
		delete(m.running, t.ID)
		m.mu.Unlock()
	}()

	if err := m.db.ClearFileIndex(t.VaultID); err != nil {
		t.Status = "error"
		t.ErrorMsg = err.Error()
		_ = m.db.UpdateTask(t)
		return
	}

	err := vault.WalkFiles("/", func(virtualPath string, info crypto.FileInfo) error {
		select {
		case <-cancel:
			return fmt.Errorf(errTaskCancelled)
		default:
		}

		t.CurrentFile = virtualPath
		_ = m.db.UpdateTask(t)

		hash, err := vault.HashVirtualFile(virtualPath)
		if err != nil {
			return fmt.Errorf("hash %s: %w", virtualPath, err)
		}

		if err := m.db.UpsertFileIndex(&storage.FileIndexRecord{
			VaultID:     t.VaultID,
			VirtualPath: virtualPath,
			ContentHash: hash,
			Size:        info.Size,
			ModTime:     info.ModTime,
		}); err != nil {
			return fmt.Errorf("index %s: %w", virtualPath, err)
		}

		t.ProcessedFiles++
		t.ProcessedBytes += info.Size
		return m.db.UpdateTask(t)
	})

	if err != nil {
		if err.Error() == errTaskCancelled {
			t.Status = "cancelled"
			t.ErrorMsg = ""
		} else {
			t.Status = "error"
			t.ErrorMsg = err.Error()
		}
	} else {
		t.Status = "done"
		t.CurrentFile = ""
	}

	_ = m.db.UpdateTask(t)
}

func (m *Manager) encryptDirRecursive(vault *crypto.Vault, keys *crypto.VaultKeys, t *storage.TaskRecord, srcPath, destPath string, cancel chan struct{}) error {
	entries, err := os.ReadDir(srcPath)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", srcPath, err)
	}

	for _, entry := range entries {
		// Check cancellation
		select {
		case <-cancel:
			return fmt.Errorf(errTaskCancelled)
		default:
		}

		entryPath := filepath.Join(srcPath, entry.Name())
		virtualPath := filepath.Join(destPath, entry.Name())

		if entry.IsDir() {
			if err := vault.CreateEncryptedDirectory(virtualPath); err != nil {
				return fmt.Errorf("create dir %s: %w", virtualPath, err)
			}
			if err := m.encryptDirRecursive(vault, keys, t, entryPath, virtualPath, cancel); err != nil {
				return err
			}
		} else {
			t.CurrentFile = entry.Name()
			_ = m.db.UpdateTask(t)

			info, _ := entry.Info()
			result, err := crypto.EncryptSingleFile(vault, keys, entryPath, virtualPath)
			if err != nil {
				// On failure (including cancellation), clean up the partially
				// written encrypted file to avoid leaving corrupted data.
				if encPath, resolveErr := vault.GetEncryptedFilePath(virtualPath); resolveErr == nil {
					os.Remove(encPath)
				}
				return fmt.Errorf("encrypt %s: %w", virtualPath, err)
			}

			if err := m.db.UpsertFileIndex(&storage.FileIndexRecord{
				VaultID:     vault.ID,
				VirtualPath: virtualPath,
				ContentHash: result.ContentHash,
				Size:        result.PlaintextSize,
				ModTime:     result.ModTime,
			}); err != nil {
				if encPath, resolveErr := vault.GetEncryptedFilePath(virtualPath); resolveErr == nil {
					parentDir := filepath.Dir(encPath)
					if strings.HasSuffix(parentDir, crypto.ShortNameDir) {
						_ = os.RemoveAll(parentDir)
					} else {
						_ = os.Remove(encPath)
					}
				}
				return fmt.Errorf("index %s: %w", virtualPath, err)
			}

			// Drop page cache for source file immediately after encryption.
			crypto.DropPathCache(entryPath)

			// Delete source file only after encryption is fully synced to disk.
			// EncryptSingleFile now fsync's before returning, so this is safe.
			if t.DeleteSource {
				_ = os.Remove(entryPath)
			}

			// Enqueue video thumbnail generation
			if m.thumbs != nil && thumbnail.IsVideo(entry.Name()) {
				m.thumbs.Enqueue(vault.ID, vault.Path, keys, virtualPath)
			}
			t.ProcessedFiles++
			if info != nil && info.Size() > 0 {
				t.ProcessedBytes += info.Size()
			} else {
				t.ProcessedBytes += result.PlaintextSize
			}
			_ = m.db.UpdateTask(t)
		}
	}
	return nil
}

func countVaultFiles(vault *crypto.Vault) (int, int64, error) {
	count := 0
	var totalBytes int64

	err := vault.WalkFiles("/", func(_ string, info crypto.FileInfo) error {
		count++
		totalBytes += info.Size
		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	return count, totalBytes, nil
}

func countFiles(dir string) (int, int64, error) {
	count := 0
	var totalBytes int64

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
			if info, err := d.Info(); err == nil {
				totalBytes += info.Size()
			}
		}
		return nil
	})
	return count, totalBytes, err
}

// GenerateID generates a random hex ID (128-bit entropy)
func GenerateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// removeEmptyDirs walks bottom-up and removes empty directories.
// Leaves non-empty directories (containing failed files) intact.
func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Reverse order = deepest first, os.Remove only succeeds on empty dirs
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
}
