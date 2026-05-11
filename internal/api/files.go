package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cryp/internal/crypto"
	"cryp/internal/session"
	"cryp/internal/storage"
	"cryp/internal/task"
	"cryp/internal/thumbnail"

	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list indexed directory: " + err.Error()})
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

type transcodeProfile struct {
	name            string
	beforeInputArgs []string
	videoArgs       []string
}

type hlsStream struct {
	dir             string
	cmd             *exec.Cmd
	cancel          context.CancelFunc
	sessionID       string
	durationSeconds float64
	segmentSeconds  float64
	playlist        string
	lastSeen        time.Time
}

var (
	ffmpegEncodersOnce  sync.Once
	ffmpegEncodersCache map[string]bool
)

func (s *Server) handleHLSStart(c *gin.Context) {
	sess := getSession(c)
	rawPath := c.Query("path")
	if rawPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}
	path := crypto.NormalizeVirtualPath(rawPath)

	keysCopy := sess.Keys.Clone()
	sessionID, err := s.sessions.Create(sess.VaultID, sess.VaultPath, keysCopy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create hls session"})
		return
	}

	contentURL := fmt.Sprintf("http://127.0.0.1:%s/api/vaults/%s/files/content?probe=1&path=%s",
		os.Getenv("PORT"), sess.VaultID, url.QueryEscape(path))
	if os.Getenv("PORT") == "" {
		contentURL = fmt.Sprintf("http://127.0.0.1:9527/api/vaults/%s/files/content?probe=1&path=%s",
			sess.VaultID, url.QueryEscape(path))
	}

	streamID, err := randomHex(16)
	if err != nil {
		s.sessions.Delete(sessionID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create stream id"})
		return
	}
	dir := filepath.Join(os.TempDir(), "cryp-hls-"+streamID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.sessions.Delete(sessionID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create hls temp dir"})
		return
	}

	stream, err := s.startHLSStream(context.Background(), dir, contentURL, sessionID)
	if err != nil {
		s.sessions.Delete(sessionID)
		os.RemoveAll(dir)
		log.Printf("hls: failed for %s: %v", path, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start hls transcode"})
		return
	}

	s.hlsMu.Lock()
	s.hls[streamID] = stream
	s.hlsMu.Unlock()

	go s.cleanupHLSStream(streamID, stream)

	c.Redirect(http.StatusFound, fmt.Sprintf("/api/vaults/%s/files/hls/%s/index.m3u8", sess.VaultID, streamID))
}

func (s *Server) startHLSStream(ctx context.Context, dir, contentURL, sessionID string) (*hlsStream, error) {
	durationSeconds := probeMediaDuration(ctx, contentURL, sessionID)
	segmentSeconds := 2.0
	playlist := ""
	if durationSeconds > 0 {
		playlist = buildVODPlaylist(durationSeconds, segmentSeconds)
	}
	profiles := buildHLSProfiles()
	var lastErr error
	for _, profile := range profiles {
		started := time.Now()
		cmd, cancel, stderr, err := startHLSCommand(ctx, profile, dir, contentURL, sessionID)
		if err != nil {
			lastErr = err
			continue
		}
		if err := waitForHLSReady(ctx, cmd, dir, 5*time.Second); err != nil {
			cancel()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			lastErr = fmt.Errorf("hls %s failed: %w stderr=%s", profile.name, err, trimLog(stderr.String()))
			continue
		}
		log.Printf("hls: started profile=%s in %s", profile.name, time.Since(started).Round(time.Millisecond))
		return &hlsStream{
			dir:             dir,
			cmd:             cmd,
			cancel:          cancel,
			sessionID:       sessionID,
			durationSeconds: durationSeconds,
			segmentSeconds:  segmentSeconds,
			playlist:        playlist,
			lastSeen:        time.Now(),
		}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no hls profile available")
}

func startHLSCommand(ctx context.Context, profile transcodeProfile, dir, contentURL, sessionID string) (*exec.Cmd, context.CancelFunc, *bytes.Buffer, error) {
	segmentPattern := filepath.Join(dir, "segment_%05d.ts")
	playlistPath := filepath.Join(dir, "index.m3u8")
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-probesize", "10M",
		"-analyzeduration", "10M",
	}
	args = append(args, profile.beforeInputArgs...)
	args = append(args,
		"-re",
		"-headers", fmt.Sprintf("Cookie: session_id=%s\r\n", sessionID),
		"-i", contentURL,
		"-map", "0:v:0",
		"-map", "0:a:0?",
	)
	args = append(args, profile.videoArgs...)
	args = append(args,
		"-c:a", "aac",
		"-b:a", "128k",
		"-force_key_frames", "expr:gte(t,n_forced*2)",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_filename", segmentPattern,
		playlistPath,
	)

	streamCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(streamCtx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, nil, err
	}
	cmd.Cancel = func() error {
		cancel()
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	return cmd, cancel, &stderr, nil
}

func waitForHLSReady(ctx context.Context, cmd *exec.Cmd, dir string, timeout time.Duration) error {
	playlistPath := filepath.Join(dir, "index.m3u8")
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("playlist timeout")
		case <-ticker.C:
			data, err := os.ReadFile(playlistPath)
			if err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					line = strings.TrimSpace(line)
					if strings.HasSuffix(line, ".ts") {
						if _, statErr := os.Stat(filepath.Join(dir, filepath.Base(line))); statErr == nil {
							return nil
						}
					}
				}
			}
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				return fmt.Errorf("ffmpeg exited")
			}
		}
	}
}

func (s *Server) cleanupHLSStream(streamID string, stream *hlsStream) {
	done := make(chan error, 1)
	go func() {
		done <- stream.cmd.Wait()
	}()

	var err error
	stoppedForIdle := false
	idleTicker := time.NewTicker(30 * time.Second)
	defer idleTicker.Stop()
	for {
		select {
		case err = <-done:
			goto finished
		case <-idleTicker.C:
			s.hlsMu.Lock()
			idleFor := time.Since(stream.lastSeen)
			s.hlsMu.Unlock()
			if idleFor > 5*time.Minute {
				stoppedForIdle = true
				stream.cancel()
			}
		}
	}

finished:
	if err != nil && !stoppedForIdle {
		log.Printf("hls: stream %s ended: %v", streamID, err)
	}
	s.sessions.Delete(stream.sessionID)

	if !stoppedForIdle {
		time.Sleep(15 * time.Minute)
	}
	s.hlsMu.Lock()
	delete(s.hls, streamID)
	s.hlsMu.Unlock()
	os.RemoveAll(stream.dir)
}

func (s *Server) handleHLSAsset(c *gin.Context) {
	streamID := c.Param("stream")
	name := filepath.Base(c.Param("name"))
	if name == "." || name == string(filepath.Separator) || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid hls asset"})
		return
	}

	s.hlsMu.Lock()
	stream := s.hls[streamID]
	if stream != nil {
		stream.lastSeen = time.Now()
	}
	s.hlsMu.Unlock()
	if stream == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "hls stream not found"})
		return
	}

	path := filepath.Join(stream.dir, name)
	if name == "index.m3u8" {
		c.Header("Content-Type", "application/vnd.apple.mpegurl")
		c.Header("Cache-Control", "no-cache, max-age=1")
		if stream.playlist != "" {
			c.String(http.StatusOK, stream.playlist)
			return
		}
		c.File(path)
		return
	}
	if strings.HasSuffix(name, ".ts") {
		if err := waitForFile(c.Request.Context(), path, 30*time.Second); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "hls segment not ready"})
			return
		}
		c.Header("Content-Type", "video/mp2t")
		c.Header("Cache-Control", "public, max-age=300")
		c.File(path)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "hls asset not found"})
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func probeMediaDuration(ctx context.Context, contentURL, sessionID string) float64 {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, "ffprobe",
		"-v", "error",
		"-probesize", "10M",
		"-analyzeduration", "10M",
		"-headers", fmt.Sprintf("Cookie: session_id=%s\r\n", sessionID),
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		contentURL,
	).Output()
	if err != nil {
		log.Printf("hls: duration probe failed: %v", err)
		return 0
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return 0
	}
	return duration
}

func buildVODPlaylist(durationSeconds, segmentSeconds float64) string {
	segmentCount := int(math.Ceil(durationSeconds / segmentSeconds))
	if segmentCount < 1 {
		segmentCount = 1
	}
	targetDuration := int(math.Ceil(segmentSeconds))
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i := 0; i < segmentCount; i++ {
		segmentDuration := segmentSeconds
		remaining := durationSeconds - float64(i)*segmentSeconds
		if remaining < segmentDuration {
			segmentDuration = remaining
		}
		if segmentDuration <= 0 {
			segmentDuration = segmentSeconds
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", segmentDuration))
		b.WriteString(fmt.Sprintf("segment_%05d.ts\n", i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("segment timeout")
		case <-ticker.C:
		}
	}
}

func buildHLSProfiles() []transcodeProfile {
	requested := strings.ToLower(strings.TrimSpace(os.Getenv("CRYP_FFMPEG_HWACCEL")))
	if requested == "none" || requested == "off" || requested == "cpu" {
		return []transcodeProfile{cpuTranscodeProfile()}
	}

	encoders := detectFFmpegEncoders()
	profiles := make([]transcodeProfile, 0, 3)
	if requested == "vaapi-hwdec" {
		if profile, ok := gpuTranscodeProfile("vaapi", encoders); ok {
			profiles = append(profiles, profile)
		}
	}
	if requested == "" || requested == "auto" || requested == "vaapi" || requested == "vaapi-hwdec" {
		if profile, ok := gpuFallbackTranscodeProfile("vaapi", encoders); ok {
			profiles = append(profiles, profile)
		}
	}
	profiles = append(profiles, cpuTranscodeProfile())
	return profiles
}

func gpuTranscodeProfile(name string, encoders map[string]bool) (transcodeProfile, bool) {
	switch name {
	case "vaapi":
		if !encoders["h264_vaapi"] {
			return transcodeProfile{}, false
		}
		if _, err := os.Stat("/dev/dri/renderD128"); err != nil {
			return transcodeProfile{}, false
		}
		return transcodeProfile{
			name: "vaapi-hwdec",
			beforeInputArgs: []string{
				"-hwaccel", "vaapi",
				"-hwaccel_device", "/dev/dri/renderD128",
				"-hwaccel_output_format", "vaapi",
			},
			videoArgs: []string{"-vf", "scale_vaapi=format=nv12", "-c:v", "h264_vaapi", "-qp", "24"},
		}, true
	default:
		return transcodeProfile{}, false
	}
}

func gpuFallbackTranscodeProfile(name string, encoders map[string]bool) (transcodeProfile, bool) {
	switch name {
	case "vaapi":
		if !encoders["h264_vaapi"] {
			return transcodeProfile{}, false
		}
		if _, err := os.Stat("/dev/dri/renderD128"); err != nil {
			return transcodeProfile{}, false
		}
		return transcodeProfile{
			name:            "vaapi",
			beforeInputArgs: []string{"-vaapi_device", "/dev/dri/renderD128"},
			videoArgs:       []string{"-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-qp", "24"},
		}, true
	default:
		return transcodeProfile{}, false
	}
}

func cpuTranscodeProfile() transcodeProfile {
	return transcodeProfile{
		name:      "cpu",
		videoArgs: []string{"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p"},
	}
}

func detectFFmpegEncoders() map[string]bool {
	ffmpegEncodersOnce.Do(func() {
		ffmpegEncodersCache = make(map[string]bool)
		out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").CombinedOutput()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ffmpegEncodersCache[fields[1]] = true
			}
		}
	})
	return ffmpegEncodersCache
}

func trimLog(s string) string {
	if len(s) > 500 {
		return s[len(s)-500:]
	}
	return s
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

	hasher := sha256.New()
	// Stream directly: multipart body -> encrypting writer -> output file
	// Only drop output file cache (network stream has no page cache)
	written, err := crypto.StreamCopyWithOutputCacheDrop(io.MultiWriter(writer, hasher), part, outFile)
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

	modTime := outFileStatModTime(outFile)
	protectedHash := crypto.ProtectContentHash(sess.Keys.MACKey, sess.VaultID, hex.EncodeToString(hasher.Sum(nil)))
	if err := s.upsertEntry(sess.Keys.MACKey, sess.VaultID, virtualPath, false, false, written, modTime, protectedHash); err != nil {
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

	// Enqueue video thumbnail generation
	if s.thumbs != nil && written > 0 && (thumbnail.IsVideo(fileName) || thumbnail.IsHEIF(fileName)) {
		s.thumbs.Enqueue(sess.VaultID, sess.VaultPath, sess.Keys, virtualPath)
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

	vault := &crypto.Vault{
		ID:   sess.VaultID,
		Path: sess.VaultPath,
		Keys: sess.Keys,
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

	deleted := make([]string, 0, len(req.Paths))
	failed := make(map[string]string)
	for _, path := range req.Paths {
		if path == "" {
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

	taskID, err := task.GenerateID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate task id"})
		return
	}
	if err := s.tasks.StartRebuildIndex(taskID, sess.VaultID, sess.VaultPath, sess.Keys); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start rebuild task: " + err.Error()})
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
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}

	thumbPath := s.thumbs.GetPath(sess.VaultID, path)
	if thumbPath == "" {
		// Thumbnail not yet generated — trigger async generation and return 404
		if s.thumbs != nil {
			s.thumbs.Enqueue(sess.VaultID, sess.VaultPath, sess.Keys, path)
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "thumbnail not ready"})
		return
	}

	// Decrypt the encrypted thumbnail and stream it back
	reader, release, err := thumbnail.DecryptThumbnail(thumbPath, sess.Keys.MasterKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt thumbnail failed"})
		return
	}
	defer release()

	c.Header("Cache-Control", "public, max-age=86400")
	c.Header("Content-Type", "image/jpeg")
	c.Status(http.StatusOK)
	io.Copy(c.Writer, reader)
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
	normalized := crypto.NormalizeVirtualPath(virtualPath)
	pathKey, err := crypto.EncryptIndexPath(macKey, vaultID, normalized)
	if err != nil {
		return nil, err
	}
	parent := crypto.ParentVirtualPath(normalized)
	parentKey := ""
	if parent != "" {
		parentKey, err = crypto.EncryptIndexPath(macKey, vaultID, parent)
		if err != nil {
			return nil, err
		}
	}
	nameKey, err := crypto.EncryptEntryNameKey(macKey, vaultID, parentKey, crypto.BaseVirtualName(normalized))
	if err != nil {
		return nil, err
	}
	if isDir {
		protectedHash = ""
		size = 0
	}
	return &storage.EntryRecord{
		VaultID:         vaultID,
		PathKey:         pathKey,
		ParentKey:       parentKey,
		NameKey:         nameKey,
		IsDir:           isDir,
		ChildrenIndexed: childrenIndexed,
		ContentHash:     protectedHash,
		Size:            size,
		ModTime:         modTime,
	}, nil
}
