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
	_, err := d.Exec(`
		CREATE TABLE IF NOT EXISTS vaults (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			path TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);

		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			vault_id TEXT,
			data BLOB,
			expires_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

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
	`)
	return err
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
	_, err := d.Exec("DELETE FROM vaults WHERE id = ?", id)
	return err
}

// CleanExpiredSessions removes expired sessions
func (d *DB) CleanExpiredSessions() error {
	_, err := d.Exec("DELETE FROM sessions WHERE expires_at < ?", time.Now().Unix())
	return err
}

// TaskRecord represents a background task in the database
type TaskRecord struct {
	ID             string `json:"id"`
	VaultID        string `json:"vaultId"`
	Type           string `json:"type"` // "import" or "upload"
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
		t.ID, t.VaultID, t.Type, t.Status, t.TotalFiles, t.ProcessedFiles, t.TotalBytes, t.ProcessedBytes, t.CurrentFile, t.ErrorMsg, t.SourcePath, t.DestPath, delSrc, t.StartedAt, t.CreatedAt, t.UpdatedAt,
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
		t.Status, t.TotalFiles, t.ProcessedFiles, t.TotalBytes, t.ProcessedBytes, t.CurrentFile, t.ErrorMsg, t.StartedAt, t.UpdatedAt, delSrc, t.ID,
	)
	return err
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
