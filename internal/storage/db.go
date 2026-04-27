package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// DB wraps the SQLite database
type DB struct {
	*sql.DB
}

// VaultRecord represents a vault in the database
type VaultRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// NewDB initializes the SQLite database
func NewDB(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "cryp.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	d := &DB{db}
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return d, nil
}

func (d *DB) migrate() error {
	if _, err := d.Exec(`
		CREATE TABLE IF NOT EXISTS vaults (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			path TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);

		CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			vault_id TEXT NOT NULL,
			type TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			total_files INTEGER NOT NULL DEFAULT 0,
			processed_files INTEGER NOT NULL DEFAULT 0,
			total_bytes INTEGER NOT NULL DEFAULT 0,
			processed_bytes INTEGER NOT NULL DEFAULT 0,
			current_file TEXT NOT NULL DEFAULT '',
			error_msg TEXT NOT NULL DEFAULT '',
			source_path TEXT NOT NULL DEFAULT '',
			dest_path TEXT NOT NULL DEFAULT '',
			delete_source INTEGER NOT NULL DEFAULT 0,
			started_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_tasks_vault ON tasks(vault_id);

		CREATE TABLE IF NOT EXISTS file_index (
			vault_id TEXT NOT NULL,
			virtual_path TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			size INTEGER NOT NULL DEFAULT 0,
			mod_time INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (vault_id, virtual_path)
		);

		CREATE INDEX IF NOT EXISTS idx_file_index_vault_hash ON file_index(vault_id, content_hash, size);
		CREATE INDEX IF NOT EXISTS idx_file_index_vault_path ON file_index(vault_id, virtual_path);

		CREATE TABLE IF NOT EXISTS app_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`); err != nil {
		return err
	}

	return d.migratePathPrivacy()
}

func (d *DB) migratePathPrivacy() error {
	const migrationKey = "path_privacy_v1"
	var value string
	err := d.QueryRow("SELECT value FROM app_meta WHERE key=?", migrationKey).Scan(&value)
	if err == nil && value == "done" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Existing rows used plaintext virtual paths. They cannot be encrypted
	// without the vault password, so remove them and let users rebuild the
	// duplicate index after login.
	if _, err := tx.Exec("DELETE FROM file_index"); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE tasks SET current_file='', error_msg='', source_path='', dest_path=''"); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO app_meta (key, value) VALUES (?, 'done')", migrationKey); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// Physically rewrite the database so deleted plaintext metadata is not
	// left in free pages. Ignore failure; the logical migration is complete.
	_, _ = d.Exec("VACUUM")
	_, _ = d.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return nil
}

// CreateVault inserts a new vault record
func (d *DB) CreateVault(vault *VaultRecord) error {
	now := time.Now().Unix()
	vault.CreatedAt = now
	vault.UpdatedAt = now

	_, err := d.Exec(
		"INSERT INTO vaults (id, name, path, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		vault.ID, vault.Name, vault.Path, vault.CreatedAt, vault.UpdatedAt,
	)
	return err
}

// GetVault retrieves a vault by ID
func (d *DB) GetVault(id string) (*VaultRecord, error) {
	var v VaultRecord
	err := d.QueryRow(
		"SELECT id, name, path, created_at, updated_at FROM vaults WHERE id = ?", id,
	).Scan(&v.ID, &v.Name, &v.Path, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// GetVaultByName retrieves a vault by its name
func (d *DB) GetVaultByName(name string) (*VaultRecord, error) {
	var v VaultRecord
	err := d.QueryRow(
		"SELECT id, name, path, created_at, updated_at FROM vaults WHERE name = ?", name,
	).Scan(&v.ID, &v.Name, &v.Path, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListVaults returns all vault records
func (d *DB) ListVaults() ([]VaultRecord, error) {
	rows, err := d.Query("SELECT id, name, path, created_at, updated_at FROM vaults ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vaults []VaultRecord
	for rows.Next() {
		var v VaultRecord
		if err := rows.Scan(&v.ID, &v.Name, &v.Path, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		vaults = append(vaults, v)
	}
	return vaults, rows.Err()
}

// DeleteVault removes a vault record
func (d *DB) DeleteVault(id string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM file_index WHERE vault_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM tasks WHERE vault_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM vaults WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// TaskRecord represents a background task in the database
type TaskRecord struct {
	ID             string `json:"id"`
	VaultID        string `json:"vaultId"`
	Type           string `json:"type"`   // "import" or "upload"
	Status         string `json:"status"` // "pending", "running", "done", "error", "cancelled"
	TotalFiles     int    `json:"totalFiles"`
	ProcessedFiles int    `json:"processedFiles"`
	TotalBytes     int64  `json:"totalBytes"`
	ProcessedBytes int64  `json:"processedBytes"`
	CurrentFile    string `json:"currentFile"`
	ErrorMsg       string `json:"errorMsg,omitempty"`
	SourcePath     string `json:"sourcePath,omitempty"`
	DestPath       string `json:"destPath,omitempty"`
	DeleteSource   bool   `json:"deleteSource"`
	StartedAt      int64  `json:"startedAt"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// FileIndexRecord represents an indexed vault file for duplicate detection.
type FileIndexRecord struct {
	VaultID     string `json:"vaultId"`
	VirtualPath string `json:"virtualPath"`
	ContentHash string `json:"contentHash"`
	Size        int64  `json:"size"`
	ModTime     int64  `json:"modTime"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// DuplicateGroupRow is a flattened duplicate-file row.
type DuplicateGroupRow struct {
	ContentHash string `json:"contentHash"`
	VirtualPath string `json:"virtualPath"`
	Size        int64  `json:"size"`
	ModTime     int64  `json:"modTime"`
}

// DuplicateStats summarizes duplicate files for a vault.
type DuplicateStats struct {
	GroupCount          int   `json:"groupCount"`
	FileCount           int   `json:"fileCount"`
	DuplicateFileCount  int   `json:"duplicateFileCount"`
	TotalBytes          int64 `json:"totalBytes"`
	DuplicateTotalBytes int64 `json:"duplicateTotalBytes"`
}

// CreateTask inserts a new task record
func (d *DB) CreateTask(t *TaskRecord) error {
	now := time.Now().Unix()
	t.CreatedAt = now
	t.UpdatedAt = now
	delSrc := 0
	if t.DeleteSource {
		delSrc = 1
	}
	_, err := d.Exec(
		`INSERT INTO tasks (id, vault_id, type, status, total_files, processed_files, total_bytes, processed_bytes, current_file, error_msg, source_path, dest_path, delete_source, started_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.VaultID, t.Type, t.Status, t.TotalFiles, t.ProcessedFiles, t.TotalBytes, t.ProcessedBytes, "", taskErrorForStorage(t), "", "", delSrc, t.StartedAt, t.CreatedAt, t.UpdatedAt,
	)
	return err
}

// UpdateTask updates task progress fields
func (d *DB) UpdateTask(t *TaskRecord) error {
	t.UpdatedAt = time.Now().Unix()
	delSrc := 0
	if t.DeleteSource {
		delSrc = 1
	}
	_, err := d.Exec(
		`UPDATE tasks SET status=?, total_files=?, processed_files=?, total_bytes=?, processed_bytes=?, current_file=?, error_msg=?, started_at=?, updated_at=?, delete_source=? WHERE id=?`,
		t.Status, t.TotalFiles, t.ProcessedFiles, t.TotalBytes, t.ProcessedBytes, "", taskErrorForStorage(t), t.StartedAt, t.UpdatedAt, delSrc, t.ID,
	)
	return err
}

func taskErrorForStorage(t *TaskRecord) string {
	if t != nil && t.Status == "error" && t.ErrorMsg != "" {
		return "task failed"
	}
	return ""
}

// GetTask retrieves a task by ID
func (d *DB) GetTask(id string) (*TaskRecord, error) {
	var t TaskRecord
	var delSrc int
	err := d.QueryRow(
		`SELECT id, vault_id, type, status, total_files, processed_files, total_bytes, processed_bytes, current_file, error_msg, source_path, dest_path, delete_source, started_at, created_at, updated_at FROM tasks WHERE id=?`, id,
	).Scan(&t.ID, &t.VaultID, &t.Type, &t.Status, &t.TotalFiles, &t.ProcessedFiles, &t.TotalBytes, &t.ProcessedBytes, &t.CurrentFile, &t.ErrorMsg, &t.SourcePath, &t.DestPath, &delSrc, &t.StartedAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	t.DeleteSource = delSrc != 0
	return &t, nil
}

// ListTasks returns all tasks for a vault, newest first
func (d *DB) ListTasks(vaultID string) ([]TaskRecord, error) {
	rows, err := d.Query(
		`SELECT id, vault_id, type, status, total_files, processed_files, total_bytes, processed_bytes, current_file, error_msg, source_path, dest_path, delete_source, started_at, created_at, updated_at FROM tasks WHERE vault_id=? ORDER BY created_at DESC`, vaultID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []TaskRecord
	for rows.Next() {
		var t TaskRecord
		var delSrc int
		if err := rows.Scan(&t.ID, &t.VaultID, &t.Type, &t.Status, &t.TotalFiles, &t.ProcessedFiles, &t.TotalBytes, &t.ProcessedBytes, &t.CurrentFile, &t.ErrorMsg, &t.SourcePath, &t.DestPath, &delSrc, &t.StartedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.DeleteSource = delSrc != 0
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// DeleteTask removes a task record
func (d *DB) DeleteTask(id string) error {
	_, err := d.Exec("DELETE FROM tasks WHERE id=?", id)
	return err
}

// ListRunningTasks returns all tasks with status 'running'
func (d *DB) ListRunningTasks() ([]TaskRecord, error) {
	rows, err := d.Query(
		`SELECT id, vault_id, type, status, total_files, processed_files, total_bytes, processed_bytes, current_file, error_msg, source_path, dest_path, delete_source, started_at, created_at, updated_at FROM tasks WHERE status='running'`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []TaskRecord
	for rows.Next() {
		var t TaskRecord
		var delSrc int
		if err := rows.Scan(&t.ID, &t.VaultID, &t.Type, &t.Status, &t.TotalFiles, &t.ProcessedFiles, &t.TotalBytes, &t.ProcessedBytes, &t.CurrentFile, &t.ErrorMsg, &t.SourcePath, &t.DestPath, &delSrc, &t.StartedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.DeleteSource = delSrc != 0
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// DeleteCompletedTasks removes all non-running tasks for a vault
func (d *DB) DeleteCompletedTasks(vaultID string) (int64, error) {
	result, err := d.Exec("DELETE FROM tasks WHERE vault_id=? AND status IN ('done', 'error', 'cancelled')", vaultID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// UpsertFileIndex creates or updates an indexed file row.
func (d *DB) UpsertFileIndex(record *FileIndexRecord) error {
	now := time.Now().Unix()
	if record.CreatedAt == 0 {
		record.CreatedAt = now
	}
	record.UpdatedAt = now

	_, err := d.Exec(
		`INSERT INTO file_index (vault_id, virtual_path, content_hash, size, mod_time, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(vault_id, virtual_path) DO UPDATE SET
			content_hash=excluded.content_hash,
			size=excluded.size,
			mod_time=excluded.mod_time,
			updated_at=excluded.updated_at`,
		record.VaultID, record.VirtualPath, record.ContentHash, record.Size, record.ModTime, record.CreatedAt, record.UpdatedAt,
	)
	return err
}

// DeleteFileIndex removes one indexed file row.
func (d *DB) DeleteFileIndex(vaultID, virtualPath string) error {
	_, err := d.Exec("DELETE FROM file_index WHERE vault_id=? AND virtual_path=?", vaultID, virtualPath)
	return err
}

// ClearFileIndex removes all indexed files for a vault.
func (d *DB) ClearFileIndex(vaultID string) error {
	_, err := d.Exec("DELETE FROM file_index WHERE vault_id=?", vaultID)
	return err
}

// ListDuplicateGroupRows returns flattened rows for duplicate files in paged groups.
func (d *DB) ListDuplicateGroupRows(vaultID string, offset, limit int) ([]DuplicateGroupRow, bool, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := d.Query(
		`SELECT fi.content_hash, fi.virtual_path, fi.size, fi.mod_time
		 FROM file_index fi
		 JOIN (
		 	SELECT content_hash, size
		 	FROM file_index
		 	WHERE vault_id=? AND content_hash <> ''
		 	GROUP BY content_hash, size
		 	HAVING COUNT(*) > 1
		 	ORDER BY size DESC, content_hash
		 	LIMIT ? OFFSET ?
		 ) dup
		 ON dup.content_hash = fi.content_hash AND dup.size = fi.size
		 WHERE fi.vault_id=?
		 ORDER BY fi.size DESC, fi.content_hash, fi.mod_time ASC, fi.virtual_path ASC`,
		vaultID, limit+1, offset, vaultID,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var result []DuplicateGroupRow
	groupCount := 0
	lastHash := ""
	for rows.Next() {
		var row DuplicateGroupRow
		if err := rows.Scan(&row.ContentHash, &row.VirtualPath, &row.Size, &row.ModTime); err != nil {
			return nil, false, err
		}
		if row.ContentHash != lastHash {
			groupCount++
			lastHash = row.ContentHash
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := groupCount > limit
	if !hasMore {
		return result, false, nil
	}

	extraHash := ""
	seenGroups := 0
	for _, row := range result {
		if row.ContentHash != extraHash {
			seenGroups++
			if seenGroups > limit {
				extraHash = row.ContentHash
				break
			}
			extraHash = row.ContentHash
		}
	}

	trimmed := result[:0]
	for _, row := range result {
		if hasMore && row.ContentHash == extraHash {
			break
		}
		trimmed = append(trimmed, row)
	}

	return trimmed, true, nil
}

// GetDuplicateStats returns overall duplicate statistics for a vault.
func (d *DB) GetDuplicateStats(vaultID string) (*DuplicateStats, error) {
	row := d.QueryRow(
		`SELECT
			COUNT(*) AS group_count,
			COALESCE(SUM(file_count), 0) AS file_count,
			COALESCE(SUM(size * file_count), 0) AS total_bytes,
			COALESCE(SUM(file_count - 1), 0) AS duplicate_file_count,
			COALESCE(SUM(size * (file_count - 1)), 0) AS duplicate_total_bytes
		 FROM (
			SELECT content_hash, size, COUNT(*) AS file_count
			FROM file_index
			WHERE vault_id=? AND content_hash <> ''
			GROUP BY content_hash, size
			HAVING COUNT(*) > 1
		 ) dup`,
		vaultID,
	)

	var stats DuplicateStats
	if err := row.Scan(&stats.GroupCount, &stats.FileCount, &stats.TotalBytes, &stats.DuplicateFileCount, &stats.DuplicateTotalBytes); err != nil {
		return nil, err
	}
	return &stats, nil
}
