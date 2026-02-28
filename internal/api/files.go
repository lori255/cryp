package api

import (
	"crypto/rand"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cryp/internal/crypto"

	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"
)

func (s *Server) handleListFiles(c *gin.Context) {
	sess := getSession(c)
	path := c.DefaultQuery("path", "/")

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
	}

	files, err := vault.ListDirectory(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list directory: " + err.Error()})
		return
	}

	if files == nil {
		files = []crypto.FileInfo{}
	}

	c.JSON(http.StatusOK, gin.H{
		"path":  path,
		"files": files,
	})
}

func (s *Server) handleFileContent(c *gin.Context) {
	sess := getSession(c)
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
	}

	encPath, err := vault.GetEncryptedFilePath(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve path"})
		return
	}

	file, err := os.Open(encPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to stat file"})
		return
	}

	// Read file header to get content key
	header, err := crypto.ReadFileHeader(file, sess.Keys.MasterKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read file header"})
		return
	}

	plaintextSize := crypto.CipherSize2PlaintextSize(fileInfo.Size())

	// Determine content type from decrypted filename
	ext := strings.ToLower(filepath.Ext(path))
	contentType := getContentType(ext)

	// Handle Range requests for video/audio seeking
	rangeHeader := c.GetHeader("Range")
	if rangeHeader != "" {
		s.handleRangeRequest(c, file, header, plaintextSize, contentType, rangeHeader)
		return
	}
	// Advise kernel for sequential read, don't keep in cache
	unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_SEQUENTIAL|unix.FADV_NOREUSE)

	// Full file response
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.FormatInt(plaintextSize, 10))
	c.Header("Accept-Ranges", "bytes")
	c.Status(http.StatusOK)

	reader, err := crypto.NewDecryptingReader(file, header.ContentKey, header.Nonce)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create decrypting reader"})
		return
	}
	defer reader.Release()

	bufp := crypto.CopyBufPool.Get().(*[]byte)
	defer crypto.CopyBufPool.Put(bufp)
	io.CopyBuffer(c.Writer, reader, *bufp)

	// Drop page cache after serving
	crypto.DropFileCache(file)
}

func (s *Server) handleRangeRequest(c *gin.Context, file *os.File, header *crypto.FileHeader, totalSize int64, contentType string, rangeHeader string) {
	// Parse Range header: "bytes=start-end"
	rangeHeader = strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(rangeHeader, "-", 2)
	if len(parts) != 2 {
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}

	var start, end int64
	var err error

	if parts[0] == "" {
		// Suffix range: -500 (last 500 bytes)
		suffix, _ := strconv.ParseInt(parts[1], 10, 64)
		start = totalSize - suffix
		end = totalSize - 1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
			return
		}
		if parts[1] != "" {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
				return
			}
		} else {
			end = totalSize - 1
		}
	}

	if start < 0 || start >= totalSize || end >= totalSize || start > end {
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}

	// Map plaintext offset to ciphertext offset
	cipherOffset, chunkIndex, skipBytes := crypto.PlaintextOffset2CipherOffset(start)

	// Seek to the correct position in the encrypted file
	if _, err := file.Seek(cipherOffset, io.SeekStart); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "seek failed"})
		return
	}
	// Advise kernel: sequential read, don't cache
	unix.Fadvise(int(file.Fd()), cipherOffset, 0, unix.FADV_SEQUENTIAL|unix.FADV_NOREUSE)

	// Create decrypting reader starting at the right chunk
	reader, err := crypto.NewDecryptingReaderFromChunk(file, header.ContentKey, header.Nonce, chunkIndex)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "decryption failed"})
		return
	}
	defer reader.Release()

	// Skip bytes within the first chunk
	if skipBytes > 0 {
		if _, err := io.CopyN(io.Discard, reader, int64(skipBytes)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "seek failed"})
			return
		}
	}

	contentLength := end - start + 1

	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
	c.Header("Accept-Ranges", "bytes")
	c.Status(http.StatusPartialContent)

	bufp := crypto.CopyBufPool.Get().(*[]byte)
	defer crypto.CopyBufPool.Put(bufp)
	// Use LimitReader + CopyBuffer instead of CopyN to reuse pooled buffer
	io.CopyBuffer(c.Writer, io.LimitReader(reader, contentLength), *bufp)

	// Drop page cache for the region we just read
	readEnd, _ := file.Seek(0, io.SeekCurrent)
	if readEnd > cipherOffset {
		unix.Fadvise(int(file.Fd()), cipherOffset, readEnd-cipherOffset, unix.FADV_DONTNEED)
	}
}

func (s *Server) handleUploadFile(c *gin.Context) {
	sess := getSession(c)
	uploadPath := c.DefaultQuery("path", "/")
	taskID := c.Query("taskId")
	fileIndex, _ := strconv.Atoi(c.DefaultQuery("fileIndex", "0"))
	totalFiles, _ := strconv.Atoi(c.DefaultQuery("totalFiles", "1"))

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

	// Find the "file" part
	var part *multipart.Part
	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read multipart: " + err.Error()})
			return
		}
		if p.FormName() == "file" {
			part = p
			break
		}
		p.Close()
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

	encPath, err := vault.GetEncryptedFilePath(virtualPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve path: " + err.Error()})
		return
	}

	outFile, err := os.Create(encPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create file"})
		return
	}
	defer outFile.Close()

	contentKey := make([]byte, crypto.MasterKeySize)
	if _, err := rand.Read(contentKey); err != nil {
		os.Remove(encPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate content key"})
		return
	}

	header, err := crypto.WriteFileHeader(outFile, sess.Keys.MasterKey, contentKey)
	if err != nil {
		os.Remove(encPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write header"})
		return
	}

	writer, err := crypto.NewEncryptingWriter(outFile, header.ContentKey, header.Nonce)
	if err != nil {
		os.Remove(encPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create encrypting writer"})
		return
	}

	// Stream directly: multipart body -> encrypting writer -> output file
	// Only drop output file cache (network stream has no page cache)
	written, err := crypto.StreamCopyWithOutputCacheDrop(writer, part, outFile)
	if err != nil {
		os.Remove(encPath)
		if taskID != "" {
			s.tasks.FailUploadTask(taskID, "failed to encrypt: "+fileName)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt file"})
		return
	}

	if err := writer.Close(); err != nil {
		os.Remove(encPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize encryption"})
		return
	}

	// Drop page cache for encrypted output file
	crypto.DropFileCache(outFile)

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
		"message":  "file uploaded",
		"path":     virtualPath,
		"size":     written,
		"fileName": fileName,
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

	if err := vault.CreateEncryptedDirectory(req.Path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create directory: " + err.Error()})
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

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
	}

	encPath, err := vault.GetEncryptedFilePath(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve path"})
		return
	}

	info, err := os.Stat(encPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	if info.IsDir() {
		if err := os.RemoveAll(encPath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete directory"})
			return
		}
	} else {
		// For shortened names, encPath is .c9s/contents.c9r — remove the whole .c9s dir
		parentDir := filepath.Dir(encPath)
		if strings.HasSuffix(parentDir, crypto.ShortNameDir) {
			if err := os.RemoveAll(parentDir); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete file"})
				return
			}
		} else {
			if err := os.Remove(encPath); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete file"})
				return
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "deleted", "path": path})
}

func (s *Server) handleThumbnail(c *gin.Context) {
	// For thumbnails, we decrypt the file and let the browser handle display
	// In production, you'd generate actual thumbnails with image resizing
	s.handleFileContent(c)
}

func getContentType(ext string) string {
	types := map[string]string{
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".webp": "image/webp",
		".svg":  "image/svg+xml",
		".bmp":  "image/bmp",
		".ico":  "image/x-icon",
		".mp4":  "video/mp4",
		".webm": "video/webm",
		".mkv":  "video/x-matroska",
		".avi":  "video/x-msvideo",
		".mov":  "video/quicktime",
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

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
