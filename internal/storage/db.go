package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const taskRetentionSeconds = 30 * 24 * 60 * 60

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
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	d := &DB{db}
	if err := d.migrate(); err != nil {
		_ = db.Close()
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

		CREATE TABLE IF NOT EXISTS vault_entries (
			vault_id TEXT NOT NULL,
			path_key TEXT NOT NULL,
			parent_key TEXT NOT NULL,
			name_key TEXT NOT NULL,
			is_dir INTEGER NOT NULL DEFAULT 0,
			children_indexed INTEGER NOT NULL DEFAULT 0,
			content_hash TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT 0,
			mod_time INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (vault_id, path_key)
		);

		CREATE INDEX IF NOT EXISTS idx_vault_entries_parent ON vault_entries(vault_id, parent_key, is_dir);
		CREATE INDEX IF NOT EXISTS idx_vault_entries_hash ON vault_entries(vault_id, content_hash, size);

		CREATE TABLE IF NOT EXISTS vault_file_metadata (
			vault_id TEXT NOT NULL,
			path_key TEXT NOT NULL,
			payload BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (vault_id, path_key)
		);

		CREATE TABLE IF NOT EXISTS app_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		DROP TABLE IF EXISTS file_index;
	`); err != nil {
		return err
	}
	if _, err := d.Exec("ALTER TABLE vault_entries ADD COLUMN children_indexed INTEGER NOT NULL DEFAULT 0"); err != nil && !isDuplicateColumnErr(err) {
		return err
	}

	return d.migratePathPrivacy()
}

func isDuplicateColumnErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
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

	// Existing task rows may contain plaintext path/error text from older
	// versions. Entry metadata is stored in vault_entries and is built after
	// login with vault keys.
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

	if _, err := tx.Exec("DELETE FROM vault_entries WHERE vault_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM vault_file_metadata WHERE vault_id = ?", id); err != nil {
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

// EntryRecord represents one indexed file or directory. Path-bearing fields are
// deterministic encrypted keys, not plaintext paths.
type EntryRecord struct {
	VaultID         string `json:"vaultId"`
	PathKey         string `json:"pathKey"`
	ParentKey       string `json:"parentKey"`
	NameKey         string `json:"nameKey"`
	IsDir           bool   `json:"isDir"`
	ChildrenIndexed bool   `json:"childrenIndexed"`
	ContentHash     string `json:"contentHash"`
	Size            int64  `json:"size"`
	ModTime         int64  `json:"modTime"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
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
	if err == nil {
		_, _ = d.Exec("DELETE FROM tasks WHERE vault_id=? AND status IN ('done', 'error', 'cancelled') AND updated_at < ?", t.VaultID, now-taskRetentionSeconds)
	}
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

// ListTasks returns recent tasks for a vault, newest first.
func (d *DB) ListTasks(vaultID string) ([]TaskRecord, error) {
	rows, err := d.Query(
		`SELECT id, vault_id, type, status, total_files, processed_files, total_bytes, processed_bytes, current_file, error_msg, source_path, dest_path, delete_source, started_at, created_at, updated_at FROM tasks WHERE vault_id=? ORDER BY created_at DESC LIMIT 200`, vaultID,
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

// UpsertEntry creates or updates an indexed file or directory row.
func (d *DB) UpsertEntry(record *EntryRecord) error {
	return upsertEntryExecutor(d, record)
}

type sqlExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func upsertEntryExecutor(executor sqlExecutor, record *EntryRecord) error {
	if executor == nil || record == nil {
		return errors.New("invalid index entry")
	}
	now := time.Now().Unix()
	if record.CreatedAt == 0 {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	isDir := 0
	if record.IsDir {
		isDir = 1
	}
	childrenIndexed := 0
	if record.ChildrenIndexed {
		childrenIndexed = 1
	}

	_, err := executor.Exec(
		`INSERT INTO vault_entries (vault_id, path_key, parent_key, name_key, is_dir, children_indexed, content_hash, size, mod_time, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(vault_id, path_key) DO UPDATE SET
			parent_key=excluded.parent_key,
			name_key=excluded.name_key,
			is_dir=excluded.is_dir,
			children_indexed=excluded.children_indexed,
			content_hash=excluded.content_hash,
			size=excluded.size,
			mod_time=excluded.mod_time,
			updated_at=excluded.updated_at`,
		record.VaultID, record.PathKey, record.ParentKey, record.NameKey, isDir, childrenIndexed, record.ContentHash, record.Size, record.ModTime, record.CreatedAt, record.UpdatedAt,
	)
	return err
}

// GetEntry retrieves one indexed file or directory row.
func (d *DB) GetEntry(vaultID, pathKey string) (*EntryRecord, error) {
	var e EntryRecord
	var isDir, childrenIndexed int
	err := d.QueryRow(
		`SELECT vault_id, path_key, parent_key, name_key, is_dir, children_indexed, content_hash, size, mod_time, created_at, updated_at
		 FROM vault_entries
		 WHERE vault_id=? AND path_key=?`,
		vaultID, pathKey,
	).Scan(&e.VaultID, &e.PathKey, &e.ParentKey, &e.NameKey, &isDir, &childrenIndexed, &e.ContentHash, &e.Size, &e.ModTime, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	e.IsDir = isDir != 0
	e.ChildrenIndexed = childrenIndexed != 0
	return &e, nil
}

// ListChildEntries returns direct indexed children for a parent directory key.
func (d *DB) ListChildEntries(vaultID, parentKey string) ([]EntryRecord, error) {
	rows, err := d.Query(
		`SELECT vault_id, path_key, parent_key, name_key, is_dir, children_indexed, content_hash, size, mod_time, created_at, updated_at
		 FROM vault_entries
		 WHERE vault_id=? AND parent_key=?`,
		vaultID, parentKey,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []EntryRecord
	for rows.Next() {
		var e EntryRecord
		var isDir, childrenIndexed int
		if err := rows.Scan(&e.VaultID, &e.PathKey, &e.ParentKey, &e.NameKey, &isDir, &childrenIndexed, &e.ContentHash, &e.Size, &e.ModTime, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.IsDir = isDir != 0
		e.ChildrenIndexed = childrenIndexed != 0
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// DeleteEntry removes one indexed file or directory row.
func (d *DB) DeleteEntry(vaultID, pathKey string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM vault_file_metadata WHERE vault_id=? AND path_key=?", vaultID, pathKey); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM vault_entries WHERE vault_id=? AND path_key=?", vaultID, pathKey); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearEntries removes all indexed entries and their derived metadata for a vault.
func (d *DB) ClearEntries(vaultID string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM vault_file_metadata WHERE vault_id=?", vaultID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM vault_entries WHERE vault_id=?", vaultID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) UpsertFileMetadata(vaultID, pathKey string, payload []byte) error {
	if vaultID == "" || pathKey == "" || len(payload) == 0 {
		return errors.New("invalid encrypted file metadata")
	}
	now := time.Now().Unix()
	_, err := d.Exec(
		`INSERT INTO vault_file_metadata (vault_id, path_key, payload, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(vault_id, path_key) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		vaultID, pathKey, payload, now, now,
	)
	return err
}

// UpsertEntryWithFileMetadata commits the searchable index row and its
// encrypted derived metadata as one unit. New files must never become visible
// with only half of their index state written.
func (d *DB) UpsertEntryWithFileMetadata(entry *EntryRecord, payload []byte) error {
	if entry == nil || entry.VaultID == "" || entry.PathKey == "" || len(payload) == 0 {
		return errors.New("invalid indexed file metadata")
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertEntryExecutor(tx, entry); err != nil {
		return err
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(
		`INSERT INTO vault_file_metadata (vault_id, path_key, payload, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(vault_id, path_key) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		entry.VaultID, entry.PathKey, payload, now, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertEntryWithoutFileMetadata atomically publishes a non-media file and
// removes any metadata left by a previous object at the same virtual path.
func (d *DB) UpsertEntryWithoutFileMetadata(entry *EntryRecord) error {
	if entry == nil || entry.VaultID == "" || entry.PathKey == "" {
		return errors.New("invalid indexed file")
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertEntryExecutor(tx, entry); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM vault_file_metadata WHERE vault_id=? AND path_key=?", entry.VaultID, entry.PathKey); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) GetFileMetadata(vaultID, pathKey string) ([]byte, error) {
	var payload []byte
	err := d.QueryRow(
		"SELECT payload FROM vault_file_metadata WHERE vault_id=? AND path_key=?",
		vaultID, pathKey,
	).Scan(&payload)
	return payload, err
}

func (d *DB) DeleteFileMetadata(vaultID, pathKey string) error {
	_, err := d.Exec("DELETE FROM vault_file_metadata WHERE vault_id=? AND path_key=?", vaultID, pathKey)
	return err
}

func (d *DB) PruneFileMetadata(vaultID string) error {
	_, err := d.Exec(
		`DELETE FROM vault_file_metadata
		 WHERE vault_id=? AND NOT EXISTS (
			SELECT 1 FROM vault_entries
			WHERE vault_entries.vault_id=vault_file_metadata.vault_id
			  AND vault_entries.path_key=vault_file_metadata.path_key
		)`,
		vaultID,
	)
	return err
}

// ListDuplicateGroupRows returns flattened rows for duplicate files in paged groups.
func (d *DB) ListDuplicateGroupRows(vaultID string, offset, limit int) ([]DuplicateGroupRow, bool, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := d.Query(
		`SELECT fi.content_hash, fi.path_key, fi.size, fi.mod_time
		 FROM vault_entries fi
		 JOIN (
		 	SELECT content_hash, size
		 	FROM vault_entries
		 	WHERE vault_id=? AND is_dir=0 AND content_hash <> ''
		 	GROUP BY content_hash, size
		 	HAVING COUNT(*) > 1
		 	ORDER BY size DESC, content_hash
		 	LIMIT ? OFFSET ?
		 ) dup
		 ON dup.content_hash = fi.content_hash AND dup.size = fi.size
		 WHERE fi.vault_id=?
		 ORDER BY fi.size DESC, fi.content_hash, fi.mod_time ASC, fi.path_key ASC`,
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
				FROM vault_entries
				WHERE vault_id=? AND is_dir=0 AND content_hash <> ''
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
