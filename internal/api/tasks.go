package api

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cryp/internal/pathguard"
	"cryp/internal/storage"
	"cryp/internal/task"

	"github.com/gin-gonic/gin"
)

// taskResponse deliberately omits raw worker diagnostics. TaskRecord keeps
// absolute source paths and wrapped filesystem errors for server-side
// recovery, but serializing it directly would disclose host layout and
// implementation details to any authenticated vault client.
type taskResponse struct {
	ID             string `json:"id"`
	VaultID        string `json:"vaultId"`
	Type           string `json:"type"`
	Status         string `json:"status"`
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

func (s *Server) toTaskResponse(t *storage.TaskRecord) taskResponse {
	if t == nil {
		return taskResponse{}
	}
	response := taskResponse{
		ID:             t.ID,
		VaultID:        t.VaultID,
		Type:           t.Type,
		Status:         t.Status,
		TotalFiles:     t.TotalFiles,
		ProcessedFiles: t.ProcessedFiles,
		TotalBytes:     t.TotalBytes,
		ProcessedBytes: t.ProcessedBytes,
		CurrentFile:    t.CurrentFile,
		DestPath:       t.DestPath,
		DeleteSource:   t.DeleteSource,
		StartedAt:      t.StartedAt,
		CreatedAt:      t.CreatedAt,
		UpdatedAt:      t.UpdatedAt,
	}
	if t.SourcePath != "" {
		response.SourcePath = s.safeTaskSourcePath(t.SourcePath)
	}
	if t.Status == "error" {
		// Keep the UI useful without returning wrapped os/sql errors or source
		// paths. Detailed diagnostics remain in server logs.
		response.ErrorMsg = "任务执行失败"
	}
	return response
}

func (s *Server) safeTaskSourcePath(sourcePath string) string {
	if sourcePath == "" {
		return ""
	}
	if s != nil && s.sourceGuard != nil {
		root := s.sourceGuard.Root()
		if root != "" {
			if relative, err := filepath.Rel(root, sourcePath); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return filepath.ToSlash(relative)
			}
		}
	}
	// Older task rows may contain paths outside the current source root. A
	// basename is still useful for the task panel and does not disclose the
	// host directory hierarchy.
	return filepath.Base(filepath.Clean(sourcePath))
}

// handleBrowseDir lists directories on the host filesystem (within allowed paths)
func (s *Server) handleBrowseDir(c *gin.Context) {
	dirPath := c.DefaultQuery("path", s.sourceRoot)
	if s.sourceGuard == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "source directory is not configured"})
		return
	}
	absPath, err := s.sourceGuard.ResolveDir(dirPath)
	if err != nil {
		if errors.Is(err, pathguard.ErrOutsideRoot) || errors.Is(err, pathguard.ErrProtectedPath) {
			c.JSON(http.StatusForbidden, gin.H{"error": "access denied: path is outside the source root"})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "directory not found"})
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "directory not found"})
		return
	}
	if !info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a directory"})
		return
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read directory"})
		return
	}

	type DirEntry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"isDir"`
		Size  int64  `json:"size"`
	}

	var items []DirEntry
	for _, e := range entries {
		// Do not expose symlinks as selectable import sources. ResolveDir still
		// protects direct requests, while skipping them here avoids encouraging
		// a UI flow that can never be safely imported/deleted.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if s.sourceGuard.IsReserved(filepath.Join(absPath, e.Name())) {
			continue
		}
		entry := DirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
		}
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				entry.Size = info.Size()
			}
		}
		items = append(items, entry)
	}

	// Sort: dirs first, then alphabetical
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return items[i].Name < items[j].Name
	})

	c.JSON(http.StatusOK, gin.H{
		"path":  absPath,
		"items": items,
	})
}

// handleCreateImportTask starts a background import+encrypt task
func (s *Server) handleCreateImportTask(c *gin.Context) {
	sess := getSession(c)
	if s.tasks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task manager unavailable", "code": "task_manager_unavailable"})
		return
	}

	var req struct {
		SourcePath   string `json:"sourcePath" binding:"required"`
		DestPath     string `json:"destPath"`
		DeleteSource bool   `json:"deleteSource"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sourcePath required"})
		return
	}

	if req.DestPath == "" {
		req.DestPath = "/"
	}

	if s.sourceGuard == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "source directory is not configured"})
		return
	}
	validatedSource, err := s.sourceGuard.ValidateImport(req.SourcePath, req.DeleteSource)
	if err != nil {
		switch {
		case errors.Is(err, pathguard.ErrOutsideRoot), errors.Is(err, pathguard.ErrSymlinkNotAllowed), errors.Is(err, pathguard.ErrRootDelete), errors.Is(err, pathguard.ErrProtectedPath):
			c.JSON(http.StatusForbidden, gin.H{"error": "source path is not allowed"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "source directory not found"})
		}
		return
	}

	taskID, err := task.GenerateID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate task id"})
		return
	}
	err = s.tasks.StartImport(taskID, sess.VaultID, sess.VaultPath, sess.Keys, validatedSource, req.DestPath, req.DeleteSource)
	if err != nil {
		status := http.StatusInternalServerError
		code := "task_start_failed"
		if errors.Is(err, task.ErrVaultQuiescing) {
			status = http.StatusConflict
			code = "vault_removing"
		} else if errors.Is(err, task.ErrTaskManagerClosed) {
			status = http.StatusServiceUnavailable
			code = "task_manager_unavailable"
		}
		if status >= http.StatusInternalServerError {
			// Keep implementation details in server logs; clients only need a
			// stable code to decide whether to retry or show a conflict.
			c.JSON(status, gin.H{"error": "failed to start import", "code": code})
		} else {
			c.JSON(status, gin.H{"error": "vault is being removed", "code": code})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"taskId":  taskID,
		"message": "import task started",
	})
}

// handleListTasks returns all tasks for a vault
func (s *Server) handleListTasks(c *gin.Context) {
	sess := getSession(c)

	tasks, err := s.db.ListTasks(sess.VaultID)
	if err != nil {
		log.Printf("tasks: list vault=%s: %v", sess.VaultID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks", "code": "task_list_failed"})
		return
	}

	if tasks == nil {
		tasks = []storage.TaskRecord{}
	}
	responses := make([]taskResponse, 0, len(tasks))
	for i := range tasks {
		responses = append(responses, s.toTaskResponse(&tasks[i]))
	}
	c.JSON(http.StatusOK, gin.H{"tasks": responses})
}

// handleGetTask returns a single task's status
func (s *Server) handleGetTask(c *gin.Context) {
	sess := getSession(c)
	taskID := c.Param("taskId")
	t, ok := s.taskForVault(c, sess.VaultID, taskID)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, s.toTaskResponse(t))
}

// handleCancelTask cancels a running task
func (s *Server) handleCancelTask(c *gin.Context) {
	sess := getSession(c)
	taskID := c.Param("taskId")
	if s.tasks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task manager unavailable", "code": "task_manager_unavailable"})
		return
	}
	_, ok := s.taskForVault(c, sess.VaultID, taskID)
	if !ok {
		return
	}

	if s.tasks.CancelTask(taskID) {
		c.JSON(http.StatusOK, gin.H{"message": "task cancelled"})
	} else {
		// Re-read after the failed cancellation. The worker may have finished
		// between the ownership lookup and CancelTask; never overwrite a newer
		// done/error state using the stale record.
		latest, readErr := s.db.GetTask(taskID)
		if readErr != nil {
			if errors.Is(readErr, sql.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			} else {
				log.Printf("task %s: reload after cancel: %v", taskID, readErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read task", "code": "task_lookup_failed"})
			}
			return
		}
		switch latest.Status {
		case "running", "pending":
			c.JSON(http.StatusConflict, gin.H{"error": "task is finishing; retry cancellation", "code": "task_not_cancellable"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "task is not running", "code": "task_not_running"})
		}
	}
}

// handleDeleteTask deletes a completed/failed task record
func (s *Server) handleDeleteTask(c *gin.Context) {
	sess := getSession(c)
	taskID := c.Param("taskId")
	t, ok := s.taskForVault(c, sess.VaultID, taskID)
	if !ok {
		return
	}

	// Pending tasks are also owned by a future worker in older deployments;
	// deleting their row would let the worker recreate/update it after this
	// handler returns. Require cancellation/settling first.
	if t.Status == "running" || t.Status == "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot delete active task, cancel it first", "code": "task_active"})
		return
	}

	if err := s.db.DeleteTask(taskID); err != nil {
		log.Printf("task %s: delete: %v", taskID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete task", "code": "task_delete_failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "task deleted"})
}

func (s *Server) handleCreateUploadTask(c *gin.Context) {
	sess := getSession(c)
	if s.tasks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task manager unavailable", "code": "task_manager_unavailable"})
		return
	}

	var req struct {
		TotalFiles int   `json:"totalFiles" binding:"required"`
		TotalBytes int64 `json:"totalBytes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "totalFiles required"})
		return
	}
	if req.TotalFiles < 0 || req.TotalBytes < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upload totals must be non-negative", "code": "task_metadata_invalid"})
		return
	}

	taskID, err := task.GenerateID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate task id"})
		return
	}
	if err := s.tasks.CreateUploadTask(taskID, sess.VaultID, req.TotalFiles, req.TotalBytes); err != nil {
		status := http.StatusInternalServerError
		code := "task_start_failed"
		message := "failed to create upload task"
		if errors.Is(err, task.ErrVaultQuiescing) {
			status = http.StatusConflict
			code = "vault_removing"
			message = "vault is being removed"
		} else if errors.Is(err, task.ErrTaskManagerClosed) {
			status = http.StatusServiceUnavailable
			code = "task_manager_unavailable"
			message = "task manager unavailable"
		}
		c.JSON(status, gin.H{"error": message, "code": code})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"taskId":  taskID,
		"message": "upload task created",
	})
}

func (s *Server) taskForVault(c *gin.Context, vaultID, taskID string) (*storage.TaskRecord, bool) {
	t, err := s.db.GetTask(taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		} else {
			log.Printf("task %s: lookup: %v", taskID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read task", "code": "task_lookup_failed"})
		}
		return nil, false
	}
	if t.VaultID != vaultID {
		c.JSON(http.StatusForbidden, gin.H{"error": "task not for this vault"})
		return nil, false
	}
	return t, true
}

// handleDeleteCompletedTasks deletes all completed/failed/cancelled tasks for a vault
func (s *Server) handleDeleteCompletedTasks(c *gin.Context) {
	sess := getSession(c)

	count, err := s.db.DeleteCompletedTasks(sess.VaultID)
	if err != nil {
		log.Printf("tasks: delete completed vault=%s: %v", sess.VaultID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete tasks", "code": "task_cleanup_failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": count})
}
