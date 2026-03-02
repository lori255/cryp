package api

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cryp/internal/storage"
	"cryp/internal/task"

	"github.com/gin-gonic/gin"
)

// handleBrowseDir lists directories on the host filesystem (within allowed paths)
func (s *Server) handleBrowseDir(c *gin.Context) {
	dirPath := c.DefaultQuery("path", "/data")

	// Security: only allow browsing under /data
	absPath, err := filepath.Abs(dirPath)
	if err != nil || (absPath != "/data" && !strings.HasPrefix(absPath, "/data/")) {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied: can only browse /data"})
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

	// Verify source exists
	info, err := os.Stat(req.SourcePath)
	if err != nil || !info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source directory not found"})
		return
	}

	taskID, err := task.GenerateID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate task id"})
		return
	}
	err = s.tasks.StartImport(taskID, sess.VaultID, sess.VaultPath, sess.Keys, req.SourcePath, req.DestPath, req.DeleteSource)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start import: " + err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks"})
		return
	}

	if tasks == nil {
		tasks = []storage.TaskRecord{}
	}

	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

// handleGetTask returns a single task's status
func (s *Server) handleGetTask(c *gin.Context) {
	sess := getSession(c)
	taskID := c.Param("taskId")

	t, err := s.db.GetTask(taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if t.VaultID != sess.VaultID {
		c.JSON(http.StatusForbidden, gin.H{"error": "task not for this vault"})
		return
	}

	c.JSON(http.StatusOK, t)
}

// handleCancelTask cancels a running task
func (s *Server) handleCancelTask(c *gin.Context) {
	sess := getSession(c)
	taskID := c.Param("taskId")
	t, err := s.db.GetTask(taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if t.VaultID != sess.VaultID {
		c.JSON(http.StatusForbidden, gin.H{"error": "task not for this vault"})
		return
	}

	if s.tasks.CancelTask(taskID) {
		c.JSON(http.StatusOK, gin.H{"message": "task cancelled"})
	} else {
		// Task might not be running, try to update status anyway
		if t.Status == "running" || t.Status == "pending" {
			t.Status = "cancelled"
			_ = s.db.UpdateTask(t)
			c.JSON(http.StatusOK, gin.H{"message": "task cancelled"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "task not running"})
		}
	}
}

// handleDeleteTask deletes a completed/failed task record
func (s *Server) handleDeleteTask(c *gin.Context) {
	sess := getSession(c)
	taskID := c.Param("taskId")

	t, err := s.db.GetTask(taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if t.VaultID != sess.VaultID {
		c.JSON(http.StatusForbidden, gin.H{"error": "task not for this vault"})
		return
	}

	// Don't allow deleting running tasks
	if t.Status == "running" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot delete running task, cancel it first"})
		return
	}

	if err := s.db.DeleteTask(taskID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete task"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "task deleted"})
}

func (s *Server) handleCreateUploadTask(c *gin.Context) {
	sess := getSession(c)

	var req struct {
		TotalFiles int   `json:"totalFiles" binding:"required"`
		TotalBytes int64 `json:"totalBytes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "totalFiles required"})
		return
	}

	taskID, err := task.GenerateID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate task id"})
		return
	}
	if err := s.tasks.CreateUploadTask(taskID, sess.VaultID, req.TotalFiles, req.TotalBytes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upload task"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"taskId":  taskID,
		"message": "upload task created",
	})
}

// handleDeleteCompletedTasks deletes all completed/failed/cancelled tasks for a vault
func (s *Server) handleDeleteCompletedTasks(c *gin.Context) {
	sess := getSession(c)

	count, err := s.db.DeleteCompletedTasks(sess.VaultID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete tasks"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": count})
}
