package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"cryp/internal/crypto"
	"cryp/internal/fileindex"
	"cryp/internal/session"
	"cryp/internal/storage"
	"cryp/internal/task"
	"cryp/internal/thumbnail"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleListFiles(c *gin.Context) {
	sess := getSession(c)
	path := crypto.NormalizeVirtualPath(c.DefaultQuery("path", "/"))
	sortField := c.DefaultQuery("sortField", "name")
	sortDirection := c.DefaultQuery("sortDirection", "asc")
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	files, ok, err := s.listIndexedFiles(sess, path)
	if err != nil {
		log.Printf("files: list indexed vault=%s path=%s: %v", sess.VaultID, path, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list indexed directory", "code": "file_list_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusOK, gin.H{
			"path":          path,
			"files":         []fileListResp{},
			"hasMore":       false,
			"nextOffset":    0,
			"indexRequired": true,
		})
		return
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}

		var result int
		switch sortField {
		case "modTime":
			switch {
			case files[i].ModTime < files[j].ModTime:
				result = -1
			case files[i].ModTime > files[j].ModTime:
				result = 1
			}
		case "size":
			switch {
			case files[i].Size < files[j].Size:
				result = -1
			case files[i].Size > files[j].Size:
				result = 1
			}
		default:
			result = strings.Compare(strings.ToLower(files[i].Name), strings.ToLower(files[j].Name))
		}

		if result == 0 {
			result = strings.Compare(strings.ToLower(files[i].Name), strings.ToLower(files[j].Name))
		}

		if sortDirection == "desc" && result != 0 {
			return result > 0
		}
		return result < 0
	})

	start := offset
	if start > len(files) {
		start = len(files)
	}
	end := start + limit
	if end > len(files) {
		end = len(files)
	}
	pageFiles := files[start:end]
	hasMore := end < len(files)

	result := make([]fileListResp, len(pageFiles))
	for i, f := range pageFiles {
		fullPath := path
		if fullPath == "/" {
			fullPath = "/" + f.Name
		} else {
			fullPath = path + "/" + f.Name
		}
		result[i] = fileListResp{
			Name:    f.Name,
			IsDir:   f.IsDir,
			Size:    f.Size,
			ModTime: f.ModTime,
		}
		if !f.IsDir && s.thumbs != nil && s.thumbs.HasThumbnail(sess.VaultID, fullPath) {
			result[i].HasThumb = true
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"path":       path,
		"files":      result,
		"hasMore":    hasMore,
		"nextOffset": end,
	})
}

type fileListResp struct {
	Name     string `json:"name"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size,omitempty"`
	ModTime  int64  `json:"modTime,omitempty"`
	HasThumb bool   `json:"hasThumb,omitempty"`
}

func (s *Server) handleUploadFile(c *gin.Context) {
	sess := getSession(c)
	uploadPath := c.DefaultQuery("path", "/")
	taskID := c.Query("taskId")
	fileIndex, _ := strconv.Atoi(c.DefaultQuery("fileIndex", "0"))
	totalFiles, _ := strconv.Atoi(c.DefaultQuery("totalFiles", "1"))
	// Take the lifecycle barrier before any task lookup or request-body work.
	// Shutdown must be able to observe an in-flight upload even when the HTTP
	// connection is closed while ownership is being checked.
	s.hlsLifeMu.Lock()
	defer s.hlsLifeMu.Unlock()
	if taskID != "" {
		if s.tasks == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task manager unavailable", "code": "task_manager_unavailable"})
			return
		}
		// A client may only report progress for a task belonging to its own
		// vault. Check ownership before consuming the multipart body; otherwise
		// an arbitrary task ID could be used to overwrite another vault's status.
		if _, ok := s.taskForVault(c, sess.VaultID, taskID); !ok {
			return
		}
	}

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
	}

	// Parse boundary from Content-Type without buffering the body
	contentType := c.GetHeader("Content-Type")
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart content-type"})
		return
	}

	reader := multipart.NewReader(c.Request.Body, params["boundary"])

	// Find the file part without buffering the request body.
	var part *multipart.Part
	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("upload: read multipart vault=%s: %v", sess.VaultID, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart body", "code": "multipart_invalid"})
			return
		}
		if p.FormName() == "file" {
			part = p
			break
		}
		_ = p.Close()
	}
	if part == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file part not found"})
		return
	}
	defer part.Close()

	fileName := part.FileName()
	if fileName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "filename required"})
		return
	}
	virtualPath := filepath.Join(uploadPath, fileName)
	// Keep new HLS starts out of the stop-and-replace window. The write lock is
	// held through encryption and indexing, so a concurrent player cannot
	// start reading the destination again before it is complete.
	if !s.requestSessionActive(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		return
	}
	// Replacing a file invalidates any in-flight transcode for the old bytes.
	// Stop it before opening the destination so a subsequent playback cannot
	// race the upload and inherit stale segments.
	if !s.stopHLSForPath(sess.VaultID, virtualPath) {
		c.Header("Retry-After", "1")
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "active video stream is still stopping; retry upload"})
		return
	}

	encPath, err := vault.GetEncryptedFilePath(virtualPath)
	if err != nil {
		log.Printf("upload: resolve encrypted path vault=%s path=%s: %v", sess.VaultID, virtualPath, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve upload path", "code": "upload_path_resolve_failed"})
		return
	}

	outFile, err := os.CreateTemp(filepath.Dir(encPath), ".cryp-upload-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create file"})
		return
	}
	tmpPath := outFile.Name()
	defer func() {
		_ = outFile.Close()
		_ = os.Remove(tmpPath)
	}()

	contentKey := make([]byte, crypto.MasterKeySize)
	defer clear(contentKey)
	if _, err := rand.Read(contentKey); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate content key"})
		return
	}

	header, err := crypto.WriteFileHeader(outFile, sess.Keys.MasterKey, contentKey)
	if err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write header"})
		return
	}

	writer, err := crypto.NewEncryptingWriter(outFile, header.ContentKey, header.Nonce)
	if err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create encrypting writer"})
		return
	}

	hasher := sha256.New()
	// Stream directly: multipart body -> encrypting writer -> output file
	// Only drop output file cache (network stream has no page cache)
	written, err := crypto.StreamCopyWithOutputCacheDrop(io.MultiWriter(writer, hasher), part, outFile)
	if err != nil {
		os.Remove(tmpPath)
		if taskID != "" {
			s.tasks.FailUploadTask(taskID, "failed to encrypt: "+fileName)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt file"})
		return
	}

	if err := writer.Close(); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize encryption"})
		return
	}

	// Drop page cache for encrypted output file
	crypto.DropFileCache(outFile)
	modTime := outFileStatModTime(outFile)
	if err := outFile.Close(); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to close file"})
		return
	}
	if err := os.Rename(tmpPath, encPath); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to replace file"})
		return
	}

	protectedHash := crypto.ProtectContentHash(sess.Keys.MACKey, sess.VaultID, hex.EncodeToString(hasher.Sum(nil)))
	metadataReady, indexErr := s.indexCommittedFile(context.Background(), sess.VaultID, sess.VaultPath, virtualPath, sess.Keys, written, modTime, protectedHash)
	if indexErr != nil {
		log.Printf("upload: derive/index %s: %v", virtualPath, indexErr)
		removeEncryptedPath(encPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to index file"})
		return
	}

	if written > 100*1024*1024 {
		crypto.ReleaseMemoryAfterLargeFile()
	}

	// Update task progress if taskId provided
	if taskID != "" {
		processed := fileIndex + 1
		s.tasks.UpdateUploadProgress(taskID, processed, written, fileName)
		if processed >= totalFiles {
			s.tasks.CompleteUploadTask(taskID)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message":       "file uploaded",
		"path":          virtualPath,
		"size":          written,
		"fileName":      fileName,
		"metadataReady": metadataReady,
	})
}

func (s *Server) handleMkdir(c *gin.Context) {
	sess := getSession(c)

	var req struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
	}
	s.hlsLifeMu.Lock()
	defer s.hlsLifeMu.Unlock()
	if !s.requestSessionActive(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		return
	}

	if err := vault.CreateEncryptedDirectory(req.Path); err != nil {
		log.Printf("mkdir: create encrypted directory vault=%s path=%s: %v", sess.VaultID, req.Path, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create directory", "code": "directory_create_failed"})
		return
	}
	if err := s.upsertEntry(sess.Keys.MACKey, sess.VaultID, req.Path, true, true, 0, 0, ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to index directory"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "directory created", "path": req.Path})
}

func (s *Server) handleDeleteFile(c *gin.Context) {
	sess := getSession(c)
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}
	s.hlsLifeMu.Lock()
	defer s.hlsLifeMu.Unlock()
	if !s.requestSessionActive(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		return
	}

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
	}

	if !s.stopHLSForPath(sess.VaultID, path) {
		c.Header("Retry-After", "1")
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "active video stream is still stopping; retry deletion"})
		return
	}
	if err := s.deleteVirtualPathWithCleanup(vault, sess.Keys.MACKey, sess.VaultID, path); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete file"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "deleted", "path": path})
}

func (s *Server) handleDeleteFilesBatch(c *gin.Context) {
	sess := getSession(c)

	var req struct {
		Paths []string `json:"paths" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Paths) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "paths required"})
		return
	}

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
	}
	s.hlsLifeMu.Lock()
	defer s.hlsLifeMu.Unlock()
	if !s.requestSessionActive(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		return
	}

	deleted := make([]string, 0, len(req.Paths))
	failed := make(map[string]string)
	for _, path := range req.Paths {
		if path == "" {
			continue
		}
		if !s.stopHLSForPath(sess.VaultID, path) {
			failed[path] = "active video stream is still stopping"
			continue
		}

		if err := s.deleteVirtualPathWithCleanup(vault, sess.Keys.MACKey, sess.VaultID, path); err != nil {
			if os.IsNotExist(err) {
				failed[path] = "file not found"
			} else {
				failed[path] = "failed to delete file"
			}
			continue
		}

		deleted = append(deleted, path)
	}

	c.JSON(http.StatusOK, gin.H{
		"deleted": deleted,
		"failed":  failed,
	})
}

func (s *Server) handleListDuplicates(c *gin.Context) {
	sess := getSession(c)
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	type duplicateFileResp struct {
		Path     string `json:"path"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		ModTime  int64  `json:"modTime"`
		HasThumb bool   `json:"hasThumb,omitempty"`
	}
	type duplicateGroupResp struct {
		ContentHash string              `json:"contentHash"`
		Size        int64               `json:"size"`
		Files       []duplicateFileResp `json:"files"`
	}

	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	rootKey, err := crypto.EncryptIndexPath(sess.Keys.MACKey, sess.VaultID, "/")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check file index"})
		return
	}
	rootEntry, err := s.db.GetEntry(sess.VaultID, rootKey)
	if err == sql.ErrNoRows || (err == nil && (!rootEntry.IsDir || !rootEntry.ChildrenIndexed)) {
		c.JSON(http.StatusOK, gin.H{
			"groups":        []duplicateGroupResp{},
			"hasMore":       false,
			"nextOffset":    0,
			"indexRequired": true,
			"stats":         storage.DuplicateStats{},
		})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check file index"})
		return
	}

	rows, hasMore, err := s.db.ListDuplicateGroupRows(sess.VaultID, offset, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list duplicates"})
		return
	}
	stats, err := s.db.GetDuplicateStats(sess.VaultID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load duplicate stats"})
		return
	}

	groupsByHash := make(map[string]*duplicateGroupResp)
	order := make([]string, 0)
	for _, row := range rows {
		group, ok := groupsByHash[row.ContentHash]
		if !ok {
			group = &duplicateGroupResp{
				ContentHash: row.ContentHash,
				Size:        row.Size,
			}
			groupsByHash[row.ContentHash] = group
			order = append(order, row.ContentHash)
		}

		virtualPath, err := crypto.DecryptIndexPath(sess.Keys.MACKey, sess.VaultID, row.VirtualPath)
		if err != nil {
			continue
		}

		item := duplicateFileResp{
			Path:    virtualPath,
			Name:    filepath.Base(virtualPath),
			Size:    row.Size,
			ModTime: row.ModTime,
		}
		if s.thumbs != nil && thumbnail.IsVideo(item.Name) && s.thumbs.HasThumbnail(sess.VaultID, virtualPath) {
			item.HasThumb = true
		}
		group.Files = append(group.Files, item)
	}

	result := make([]duplicateGroupResp, 0, len(order))
	for _, hash := range order {
		result = append(result, *groupsByHash[hash])
	}

	c.JSON(http.StatusOK, gin.H{
		"groups":     result,
		"hasMore":    hasMore,
		"nextOffset": offset + len(result),
		"stats":      stats,
	})
}

func (s *Server) handleRebuildFileIndex(c *gin.Context) {
	sess := getSession(c)
	if s.tasks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task manager unavailable", "code": "task_manager_unavailable"})
		return
	}

	taskID, err := task.GenerateID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate task id"})
		return
	}
	if err := s.tasks.StartRebuildIndex(taskID, sess.VaultID, sess.VaultPath, sess.Keys); err != nil {
		status := http.StatusInternalServerError
		code := "task_start_failed"
		if errors.Is(err, task.ErrTaskManagerClosed) {
			status = http.StatusServiceUnavailable
			code = "task_manager_unavailable"
		} else if errors.Is(err, task.ErrVaultQuiescing) {
			status = http.StatusConflict
			code = "vault_removing"
		}
		message := "failed to start rebuild task"
		if errors.Is(err, task.ErrVaultQuiescing) {
			message = "vault is being removed"
		} else if errors.Is(err, task.ErrTaskManagerClosed) {
			message = "task manager unavailable"
		}
		c.JSON(status, gin.H{"error": message, "code": code})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"taskId":  taskID,
		"message": "rebuild index task started",
	})
}

func outFileStatModTime(file *os.File) int64 {
	info, err := file.Stat()
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

func removeEncryptedPath(encPath string) {
	parentDir := filepath.Dir(encPath)
	if strings.HasSuffix(parentDir, crypto.ShortNameDir) {
		_ = os.RemoveAll(parentDir)
		return
	}
	_ = os.Remove(encPath)
}

func (s *Server) deleteVirtualPathWithCleanup(vault *crypto.Vault, macKey []byte, vaultID, virtualPath string) error {
	encPath, err := vault.ResolveExistingFilePath(virtualPath)
	if err != nil {
		return err
	}

	info, err := os.Stat(encPath)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		if _, err := deleteVirtualPath(vault, virtualPath); err != nil {
			return err
		}
		if err := s.cleanupDeletedFileMetadata(macKey, vaultID, virtualPath); err != nil {
			return err
		}
		return nil
	}

	files, err := vault.ListDirectory(virtualPath)
	if err != nil {
		return err
	}
	for _, file := range files {
		childPath := joinVirtualPath(virtualPath, file.Name)
		if err := s.deleteVirtualPathWithCleanup(vault, macKey, vaultID, childPath); err != nil {
			return err
		}
	}

	if _, err := deleteVirtualPath(vault, virtualPath); err != nil {
		return err
	}
	return s.cleanupDeletedDirectoryMetadata(macKey, vaultID, virtualPath)
}

func (s *Server) cleanupDeletedFileMetadata(macKey []byte, vaultID, virtualPath string) error {
	indexPath, err := crypto.EncryptIndexPath(macKey, vaultID, virtualPath)
	if err != nil {
		return err
	}
	if err := s.db.DeleteEntry(vaultID, indexPath); err != nil {
		return err
	}
	if s.thumbs != nil {
		s.thumbs.DeleteThumbnail(vaultID, crypto.NormalizeVirtualPath(virtualPath))
	}
	return nil
}

func (s *Server) cleanupDeletedDirectoryMetadata(macKey []byte, vaultID, virtualPath string) error {
	indexPath, err := crypto.EncryptIndexPath(macKey, vaultID, virtualPath)
	if err != nil {
		return err
	}
	return s.db.DeleteEntry(vaultID, indexPath)
}

func joinVirtualPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

func downloadBaseName(virtualPath string) string {
	normalized := crypto.NormalizeVirtualPath(virtualPath)
	if normalized == "/" {
		return ""
	}
	return crypto.BaseVirtualName(normalized)
}

func contentDisposition(filename string) string {
	fallback := strings.NewReplacer("\\", "_", "\"", "_", "\r", "_", "\n", "_").Replace(filename)
	if fallback == "" {
		fallback = "download"
	}
	return fmt.Sprintf("attachment; filename=\"%s\"; filename*=UTF-8''%s", fallback, url.PathEscape(filename))
}

func pathJoinZip(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func safeZipPath(p string) string {
	parts := strings.Split(strings.ReplaceAll(p, "\\", "/"), "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		clean = append(clean, part)
	}
	return strings.Join(clean, "/")
}

func deleteVirtualPath(vault *crypto.Vault, virtualPath string) (bool, error) {
	encPath, err := vault.ResolveExistingFilePath(virtualPath)
	if err != nil {
		return false, err
	}

	info, err := os.Stat(encPath)
	if err != nil {
		return false, err
	}

	if info.IsDir() {
		return true, os.RemoveAll(encPath)
	}

	parentDir := filepath.Dir(encPath)
	if strings.HasSuffix(parentDir, crypto.ShortNameDir) {
		return false, os.RemoveAll(parentDir)
	}

	return false, os.Remove(encPath)
}

func (s *Server) handleThumbnail(c *gin.Context) {
	sess := getSession(c)
	c.Header("Cache-Control", "private, no-store")
	c.Header("Vary", "Origin, Cookie, X-Session-ID")
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}

	if s.thumbs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "thumbnail service unavailable"})
		return
	}
	thumbPath := s.thumbs.GetPath(sess.VaultID, path)
	if thumbPath == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "thumbnail is missing; rebuild the file index to regenerate derived media",
			"code":  "thumbnail_missing",
		})
		return
	}

	// Decrypt the encrypted thumbnail and stream it back
	reader, release, err := thumbnail.DecryptThumbnail(thumbPath, sess.Keys.MasterKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt thumbnail failed"})
		return
	}
	defer release()

	c.Header("Content-Type", "image/jpeg")
	c.Status(http.StatusOK)
	if _, copyErr := io.Copy(c.Writer, reader); copyErr != nil {
		// The status line is already committed, but logging the short-read keeps
		// corrupted cache files observable instead of silently returning a 200.
		log.Printf("thumbnail: stream %s: %v", path, copyErr)
	}
}

func getContentType(ext string) string {
	types := map[string]string{
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".webp": "image/webp",
		".heic": "image/heic",
		".heif": "image/heic",
		".svg":  "image/svg+xml",
		".bmp":  "image/bmp",
		".ico":  "image/x-icon",
		".mp4":  "video/mp4",
		".webm": "video/webm",
		".mkv":  "video/x-matroska",
		".avi":  "video/x-msvideo",
		".mov":  "video/quicktime",
		".m4v":  "video/x-m4v",
		".flv":  "video/x-flv",
		".wmv":  "video/x-ms-wmv",
		".mpg":  "video/mpeg",
		".mpeg": "video/mpeg",
		".3gp":  "video/3gpp",
		".3g2":  "video/3gpp2",
		".ts":   "video/mp2t",
		".mts":  "video/mp2t",
		".m2ts": "video/mp2t",
		".vob":  "video/dvd",
		".ogv":  "video/ogg",
		".asf":  "video/x-ms-asf",
		".rm":   "application/vnd.rn-realmedia",
		".rmvb": "application/vnd.rn-realmedia-vbr",
		".divx": "video/divx",
		".f4v":  "video/x-f4v",
		".mxf":  "application/mxf",
		".h264": "video/h264",
		".h265": "video/h265",
		".hevc": "video/h265",
		".mp3":  "audio/mpeg",
		".wav":  "audio/wav",
		".ogg":  "audio/ogg",
		".flac": "audio/flac",
		".pdf":  "application/pdf",
		".txt":  "text/plain",
		".json": "application/json",
		".xml":  "application/xml",
		".zip":  "application/zip",
		".gz":   "application/gzip",
	}
	if t, ok := types[ext]; ok {
		return t
	}
	return "application/octet-stream"
}

func (s *Server) listIndexedFiles(sess *session.Session, virtualPath string) ([]crypto.FileInfo, bool, error) {
	parentKey, err := crypto.EncryptIndexPath(sess.Keys.MACKey, sess.VaultID, virtualPath)
	if err != nil {
		return nil, false, err
	}

	current, err := s.db.GetEntry(sess.VaultID, parentKey)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !current.IsDir || !current.ChildrenIndexed {
		return nil, false, nil
	}

	entries, err := s.db.ListChildEntries(sess.VaultID, parentKey)
	if err != nil {
		return nil, false, err
	}

	files := make([]crypto.FileInfo, 0, len(entries))
	for _, entry := range entries {
		path, err := crypto.DecryptIndexPath(sess.Keys.MACKey, sess.VaultID, entry.PathKey)
		if err != nil {
			return nil, false, err
		}
		files = append(files, crypto.FileInfo{
			Name:    crypto.BaseVirtualName(path),
			IsDir:   entry.IsDir,
			Size:    entry.Size,
			ModTime: entry.ModTime,
		})
	}
	return files, true, nil
}

func (s *Server) upsertEntry(macKey []byte, vaultID, virtualPath string, isDir bool, childrenIndexed bool, size, modTime int64, protectedHash string) error {
	record, err := buildEntryRecord(macKey, vaultID, virtualPath, isDir, childrenIndexed, size, modTime, protectedHash)
	if err != nil {
		return err
	}
	return s.db.UpsertEntry(record)
}

func buildEntryRecord(macKey []byte, vaultID, virtualPath string, isDir bool, childrenIndexed bool, size, modTime int64, protectedHash string) (*storage.EntryRecord, error) {
	return fileindex.BuildEntryRecord(macKey, fileindex.EntryInput{VaultID: vaultID, VirtualPath: virtualPath, IsDir: isDir, ChildrenIndexed: childrenIndexed, Size: size, ModTime: modTime, ProtectedHash: protectedHash})
}
