package api

// This file contains plaintext content/range serving and archive downloads.
// Keeping the encrypted I/O path separate from mutation/index handlers makes
// seek and cache behavior reviewable without changing the public routes.

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cryp/internal/crypto"

	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"
)

func (s *Server) handleFileContent(c *gin.Context) {
	sess := getSession(c)
	// Plaintext media is session-scoped. Prevent browsers/proxies from
	// reusing a response (or a partial range) for another authenticated user.
	c.Header("Cache-Control", "private, no-store")
	c.Header("Vary", "Origin, Cookie, X-Session-ID")
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

	encPath, err := vault.ResolveExistingFilePath(path)
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
	if c.Query("probe") == "1" {
		contentType = "application/octet-stream"
	}

	if plaintextSize == 0 {
		c.Header("Content-Type", contentType)
		c.Header("Content-Length", "0")
		c.Header("Accept-Ranges", "bytes")
		c.Status(http.StatusOK)
		return
	}

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
	var droppedUntil int64
	for {
		n, readErr := reader.Read(*bufp)
		if n > 0 {
			if _, writeErr := c.Writer.Write((*bufp)[:n]); writeErr != nil {
				return
			}

			// Periodically drop encrypted pages already consumed during long sequential reads.
			readEnd, seekErr := file.Seek(0, io.SeekCurrent)
			if seekErr == nil && readEnd-droppedUntil >= 8*1024*1024 {
				unix.Fadvise(int(file.Fd()), droppedUntil, readEnd-droppedUntil, unix.FADV_DONTNEED)
				droppedUntil = readEnd
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return
		}
	}

	// Drop page cache after serving
	crypto.DropFileCache(file)
}

func (s *Server) handleDownloadFile(c *gin.Context) {
	sess := getSession(c)
	c.Header("Cache-Control", "private, no-store")
	c.Header("Vary", "Origin, Cookie, X-Session-ID")
	rawPath := c.Query("path")
	if rawPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}
	path := crypto.NormalizeVirtualPath(rawPath)

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
	}

	encPath, err := vault.ResolveExistingFilePath(path)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	info, err := os.Stat(encPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	if !info.IsDir() {
		if err := s.downloadSingleFile(c, vault, path); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to download file"})
		}
		return
	}

	zipName := downloadBaseName(path)
	if zipName == "" {
		zipName = "vault"
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", contentDisposition(zipName+".zip"))
	c.Status(http.StatusOK)

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	if err := s.writeDirectoryZip(zw, vault, path, zipName); err != nil {
		return
	}
}

func (s *Server) downloadSingleFile(c *gin.Context, vault *crypto.Vault, virtualPath string) error {
	file, reader, plaintextSize, err := openDecryptedVirtualFile(vault, virtualPath)
	if err != nil {
		return err
	}
	defer file.Close()
	defer reader.Release()
	defer crypto.DropFileCache(file)

	name := downloadBaseName(virtualPath)
	c.Header("Content-Type", getContentType(strings.ToLower(filepath.Ext(name))))
	c.Header("Content-Length", strconv.FormatInt(plaintextSize, 10))
	c.Header("Content-Disposition", contentDisposition(name))
	c.Header("Accept-Ranges", "none")
	c.Status(http.StatusOK)

	bufp := crypto.CopyBufPool.Get().(*[]byte)
	defer crypto.CopyBufPool.Put(bufp)
	_, err = io.CopyBuffer(c.Writer, reader, *bufp)
	return err
}

func (s *Server) writeDirectoryZip(zw *zip.Writer, vault *crypto.Vault, virtualPath, zipPath string) error {
	files, err := vault.ListDirectory(virtualPath)
	if err != nil {
		return err
	}
	if len(files) == 0 && zipPath != "" {
		if _, err := zw.Create(safeZipPath(zipPath) + "/"); err != nil {
			return err
		}
		return nil
	}

	for _, file := range files {
		childVirtualPath := joinVirtualPath(virtualPath, file.Name)
		childZipPath := safeZipPath(pathJoinZip(zipPath, file.Name))
		if file.IsDir {
			if err := s.writeDirectoryZip(zw, vault, childVirtualPath, childZipPath); err != nil {
				return err
			}
			continue
		}

		writer, err := zw.Create(childZipPath)
		if err != nil {
			return err
		}
		if err := streamDecryptedVirtualFile(vault, childVirtualPath, writer); err != nil {
			return err
		}
	}
	return nil
}

func streamDecryptedVirtualFile(vault *crypto.Vault, virtualPath string, dst io.Writer) error {
	file, reader, _, err := openDecryptedVirtualFile(vault, virtualPath)
	if err != nil {
		return err
	}
	defer file.Close()
	defer reader.Release()
	defer crypto.DropFileCache(file)

	bufp := crypto.CopyBufPool.Get().(*[]byte)
	defer crypto.CopyBufPool.Put(bufp)
	_, err = io.CopyBuffer(dst, reader, *bufp)
	return err
}

func openDecryptedVirtualFile(vault *crypto.Vault, virtualPath string) (*os.File, *crypto.DecryptingReader, int64, error) {
	encPath, err := vault.ResolveExistingFilePath(virtualPath)
	if err != nil {
		return nil, nil, 0, err
	}

	file, err := os.Open(encPath)
	if err != nil {
		return nil, nil, 0, err
	}

	unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_SEQUENTIAL|unix.FADV_NOREUSE)

	fileInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, 0, err
	}

	header, err := crypto.ReadFileHeader(file, vault.Keys.MasterKey)
	if err != nil {
		file.Close()
		return nil, nil, 0, err
	}

	reader, err := crypto.NewDecryptingReader(file, header.ContentKey, header.Nonce)
	if err != nil {
		file.Close()
		return nil, nil, 0, err
	}

	return file, reader, crypto.CipherSize2PlaintextSize(fileInfo.Size()), nil
}

func (s *Server) handleRangeRequest(c *gin.Context, file *os.File, header *crypto.FileHeader, totalSize int64, contentType string, rangeHeader string) {
	rangeNotSatisfiable := func() {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", totalSize))
		c.Header("Accept-Ranges", "bytes")
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
	}

	if totalSize <= 0 {
		rangeNotSatisfiable()
		return
	}

	// Parse Range header: "bytes=start-end". FFmpeg may send multi-range probes;
	// this endpoint streams one range, so serve the first satisfiable segment.
	rangeHeader = strings.TrimSpace(strings.TrimPrefix(rangeHeader, "bytes="))
	if comma := strings.Index(rangeHeader, ","); comma >= 0 {
		rangeHeader = strings.TrimSpace(rangeHeader[:comma])
	}
	parts := strings.SplitN(rangeHeader, "-", 2)
	if len(parts) != 2 {
		rangeNotSatisfiable()
		return
	}

	var start, end int64
	var err error

	if parts[0] == "" {
		// Suffix range: -500 (last 500 bytes)
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			rangeNotSatisfiable()
			return
		}
		if suffix > totalSize {
			suffix = totalSize
		}
		start = totalSize - suffix
		end = totalSize - 1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			rangeNotSatisfiable()
			return
		}
		if parts[1] != "" {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				rangeNotSatisfiable()
				return
			}
			if end >= totalSize {
				end = totalSize - 1
			}
		} else {
			end = totalSize - 1
		}
	}

	// FFmpeg occasionally probes one byte at the logical EOF using
	// `bytes=<size>-`. Although RFC 7233 would call this unsatisfiable, serving
	// the final byte keeps probing/seek requests compatible with that client.
	if start == totalSize && totalSize > 0 {
		start = totalSize - 1
		end = totalSize - 1
	}
	if start < 0 || start >= totalSize || start > end {
		rangeNotSatisfiable()
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
	if _, err := io.CopyBuffer(c.Writer, io.LimitReader(reader, contentLength), *bufp); err != nil {
		return
	}

	// Drop page cache for the region we just read
	readEnd, _ := file.Seek(0, io.SeekCurrent)
	if readEnd > cipherOffset {
		unix.Fadvise(int(file.Fd()), cipherOffset, readEnd-cipherOffset, unix.FADV_DONTNEED)
	}
}
