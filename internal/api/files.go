package api

import (
	"archive/zip"
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
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cryp/internal/crypto"
	"cryp/internal/procgroup"
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

type transcodeProfile struct {
	name            string
	beforeInputArgs []string
	videoArgs       []string
}

type hlsKey struct {
	vaultID     string
	virtualPath string
}

type hlsStartResult struct {
	streamID string
	err      error
}

type hlsPending struct {
	key           hlsKey
	streamID      string
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	result        hlsStartResult
	stopRequested bool
	owners        map[string]struct{}
	ownerRefs     map[string]int
	doneOnce      sync.Once
}

type hlsStream struct {
	dir           string
	cmd           *exec.Cmd
	cancel        context.CancelFunc
	key           hlsKey
	vaultID       string
	virtualPath   string
	sessionID     string
	lastSeen      time.Time
	stopRequested bool
	finished      bool
	// Deprecated compatibility fields retained for older in-package tests;
	// playlists are now produced and served directly by FFmpeg.
	durationSeconds float64
	segmentSeconds  float64
	playlist        string
	stopCh          chan struct{}
	doneCh          chan struct{}
	doneOnce        sync.Once
	waitOnce        sync.Once
	waitErr         error
	assetMu         sync.RWMutex
	owners          map[string]struct{}
	ownerRefs       map[string]int
}

const (
	hlsIdleCheckInterval = 10 * time.Second
	hlsIdleTimeout       = 120 * time.Second
	hlsStartupTimeout    = 30 * time.Second
	hlsFinishedRetention = 15 * time.Minute
	hlsMaxStreams        = 3
	hlsMaxStderrBytes    = 64 * 1024
	ffmpegDetectTimeout  = 5 * time.Second
)

var (
	// Kept for source compatibility with older in-package tests. Detection no
	// longer uses sync.Once because failed probes must be retriable.
	ffmpegEncodersOnce       sync.Once
	ffmpegEncodersMu         sync.Mutex
	ffmpegEncodersCache      map[string]bool
	ffmpegEncodersCachedAt   time.Time
	ffmpegEncodersCacheValid bool
)

const ffmpegEncodersCacheTTL = time.Minute

func (s *Server) handleHLSStart(c *gin.Context) {
	sess := getSession(c)
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Origin, Cookie, X-Session-ID")
	rawPath := c.Query("path")
	if rawPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}
	path := crypto.NormalizeVirtualPath(rawPath)
	key := hlsKey{vaultID: sess.VaultID, virtualPath: path}
	requestSessionID, _ := c.Get("sessionID")
	ownerID, _ := requestSessionID.(string)

	// A stop request can win the race with a second start while the first
	// FFmpeg process is still in its readiness phase.  Do not attach the new
	// caller to that cancelled pending start: it would inherit its eventual
	// context.Canceled result and report a spurious transcode failure.  Wait
	// for the cancelled attempt to publish its result, then retry the lookup
	// and start a fresh attempt for the new owner.
	for {
		s.hlsLifeMu.RLock()
		s.hlsMu.Lock()
		if s.hls == nil {
			s.hls = make(map[string]*hlsStream)
		}
		if s.hlsPending == nil {
			s.hlsPending = make(map[hlsKey]*hlsPending)
		}
		if s.hlsClosing {
			s.hlsMu.Unlock()
			s.hlsLifeMu.RUnlock()
			c.Header("Retry-After", "1")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "video services are shutting down"})
			return
		}
		for existingID, existing := range s.hls {
			sameKey := existing.key == key || (existing.key == (hlsKey{}) && existing.vaultID == sess.VaultID && existing.virtualPath == path)
			// A naturally completed stream is retained only so the browser can
			// finish reading its VOD segments. It must not be reused for a later
			// start of the same path: the file may have been replaced meanwhile.
			if sameKey && !existing.stopRequested && !existing.finished {
				addHLSOwnerRef(&existing.owners, &existing.ownerRefs, ownerID)
				existing.lastSeen = time.Now()
				s.hlsMu.Unlock()
				s.hlsLifeMu.RUnlock()
				redirectHLSStream(c, sess.VaultID, existingID)
				return
			}
		}
		if pending := s.hlsPending[key]; pending != nil {
			// A timed-out pending start is just as unusable as an explicitly
			// stopped one.  Waiting for its completion lets the slot be released
			// before we retry, instead of returning the old deadline error.
			if !hlsPendingReusable(pending) {
				done := pending.done
				s.hlsMu.Unlock()
				s.hlsLifeMu.RUnlock()
				select {
				case <-done:
					continue
				case <-c.Request.Context().Done():
					return
				}
			}
			addHLSOwnerRef(&pending.owners, &pending.ownerRefs, ownerID)
			s.hlsMu.Unlock()
			s.hlsLifeMu.RUnlock()
			s.waitForHLSStartOwner(c, sess.VaultID, ownerID, pending)
			return
		}
		if s.hlsActive+s.hlsStarts >= hlsMaxStreams {
			s.hlsMu.Unlock()
			s.hlsLifeMu.RUnlock()
			c.Header("Retry-After", "2")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many active video streams"})
			return
		}
		streamID, err := randomHex(16)
		if err != nil {
			s.hlsMu.Unlock()
			s.hlsLifeMu.RUnlock()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create stream id"})
			return
		}
		startCtx, startCancel := context.WithTimeout(context.Background(), hlsStartupTimeout)
		pending := &hlsPending{
			key:       key,
			streamID:  streamID,
			ctx:       startCtx,
			cancel:    startCancel,
			done:      make(chan struct{}),
			owners:    make(map[string]struct{}),
			ownerRefs: make(map[string]int),
		}
		addHLSOwnerRef(&pending.owners, &pending.ownerRefs, ownerID)
		s.hlsPending[key] = pending
		s.hlsStarts++
		s.hlsMu.Unlock()
		s.hlsLifeMu.RUnlock()

		// Run startup independently of the HTTP handler, then let the same
		// owner-aware waiter used by joined requests observe client disconnects.
		// If this is the last owner, waitForHLSStartOwner releases it and cancels
		// pending.ctx; shared callers can continue the startup safely.
		keysCopy := sess.Keys.Clone()
		go s.runHLSStart(pending, sess.VaultID, sess.VaultPath, path, keysCopy)
		s.waitForHLSStartOwner(c, sess.VaultID, ownerID, pending)
		return
	}
}

// runHLSStart performs the slow, cancellable part of HLS startup outside the
// HTTP handler.  Keeping it behind the pending owner barrier means a client
// disconnect cancels an unshared startup without cancelling a start that has
// already acquired another owner.
func (s *Server) runHLSStart(pending *hlsPending, vaultID, vaultPath, virtualPath string, keysCopy *crypto.VaultKeys) {
	if pending == nil {
		zeroHLSKeys(keysCopy)
		return
	}
	if pending.cancel != nil {
		defer pending.cancel()
	}
	defer zeroHLSKeys(keysCopy)
	if pending.ctx != nil {
		if err := pending.ctx.Err(); err != nil {
			s.completeHLSStart(pending, nil, err)
			return
		}
	}
	startCtx := pending.ctx
	if startCtx == nil {
		startCtx = context.Background()
	}

	if s.sessions == nil {
		err := errors.New("session store unavailable")
		s.completeHLSStart(pending, nil, err)
		return
	}
	sessionID, err := s.sessions.Create(vaultID, vaultPath, keysCopy)
	if err != nil {
		s.completeHLSStart(pending, nil, err)
		return
	}

	contentURL := s.hlsContentURL(vaultID, virtualPath)
	dir := filepath.Join(os.TempDir(), "cryp-hls-"+pending.streamID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.sessions.Delete(sessionID)
		s.completeHLSStart(pending, nil, err)
		return
	}

	stream, err := s.startHLSStream(startCtx, vaultID, virtualPath, dir, contentURL, sessionID)
	if err != nil {
		s.sessions.Delete(sessionID)
		os.RemoveAll(dir)
		log.Printf("hls: failed for %s: %v", virtualPath, err)
		s.completeHLSStart(pending, nil, err)
		return
	}

	result := s.completeHLSStart(pending, stream, nil)
	if result.err != nil {
		stopAndWaitHLS(stream)
		s.sessions.Delete(sessionID)
		os.RemoveAll(dir)
	}
}

func hlsPendingReusable(pending *hlsPending) bool {
	if pending == nil || pending.stopRequested {
		return false
	}
	return pending.ctx == nil || pending.ctx.Err() == nil
}

func zeroHLSKeys(keys *crypto.VaultKeys) {
	if keys == nil {
		return
	}
	for i := range keys.MasterKey {
		keys.MasterKey[i] = 0
	}
	for i := range keys.MACKey {
		keys.MACKey[i] = 0
	}
}

func (s *Server) hlsContentURL(vaultID, virtualPath string) string {
	port := s.port
	if strings.TrimSpace(port) == "" {
		port = serverPortFromEnv()
	}
	return fmt.Sprintf("http://127.0.0.1:%s/api/vaults/%s/files/content?probe=1&path=%s",
		port, vaultID, url.QueryEscape(virtualPath))
}

func redirectHLSStream(c *gin.Context, vaultID, streamID string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Origin, Cookie, X-Session-ID")
	c.Redirect(http.StatusFound, fmt.Sprintf("/api/vaults/%s/files/hls/%s/index.m3u8", vaultID, streamID))
}

// waitForHLSStart is kept as a compatibility wrapper for tests/callers that
// only need to await a pending result without owner bookkeeping.
func (s *Server) waitForHLSStart(c *gin.Context, vaultID string, pending *hlsPending) {
	s.waitForHLSStartOwner(c, vaultID, "", pending)
}

func (s *Server) waitForHLSStartOwner(c *gin.Context, vaultID, ownerID string, pending *hlsPending) {
	select {
	case <-pending.done:
		if c.Request.Context().Err() != nil {
			s.releaseHLSStartOwner(vaultID, ownerID, pending.streamID)
			return
		}
		result := pending.result
		if result.err != nil {
			c.JSON(hlsStartHTTPStatus(result.err), gin.H{"error": "failed to start hls transcode"})
			return
		}
		// The request can be cancelled in the tiny interval between the select
		// wake-up and writing the redirect. Re-check before attaching the caller
		// to the stream so a disconnect cannot strand its last owner.
		if c.Request.Context().Err() != nil {
			s.releaseHLSStartOwner(vaultID, ownerID, result.streamID)
			return
		}
		redirectHLSStream(c, vaultID, result.streamID)
	case <-c.Request.Context().Done():
		s.releaseHLSStartOwner(vaultID, ownerID, pending.streamID)
		return
	}
}

func (s *Server) releaseHLSStartOwner(vaultID, ownerID, streamID string) {
	if ownerID == "" {
		return
	}
	targets := s.collectHLSStopTargetsForOwner(vaultID, ownerID, streamID, "")
	cancelHLSStopTargets(targets)
}

func hlsStartHTTPStatus(err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		return http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// completeHLSStart atomically publishes a ready stream or releases a pending
// slot. The FFmpeg command is deliberately detached from the startup context
// once ready, so cancelling the request cannot kill a stream that was accepted.
func (s *Server) completeHLSStart(pending *hlsPending, stream *hlsStream, startErr error) hlsStartResult {
	result := hlsStartResult{err: startErr}
	accepted := false
	s.hlsMu.Lock()
	if pending.done == nil {
		pending.done = make(chan struct{})
	}
	if s.hls == nil {
		s.hls = make(map[string]*hlsStream)
	}
	if s.hlsPending == nil {
		s.hlsPending = make(map[hlsKey]*hlsPending)
	}
	current := s.hlsPending[pending.key]
	if current == pending {
		delete(s.hlsPending, pending.key)
		if s.hlsStarts > 0 {
			s.hlsStarts--
		}
		cancelled := pending.stopRequested || (pending.ctx != nil && pending.ctx.Err() != nil)
		if stream != nil && startErr == nil && !cancelled {
			stream.key = pending.key
			stream.stopCh = make(chan struct{})
			stream.doneCh = make(chan struct{})
			stream.owners = cloneHLSOwners(pending.owners)
			stream.ownerRefs = cloneHLSOwnerRefs(pending.ownerRefs)
			s.hls[pending.streamID] = stream
			s.hlsActive++
			result.streamID = pending.streamID
			accepted = true
		} else if startErr == nil {
			result.err = context.Canceled
		}
		pending.result = result
		pending.doneOnce.Do(func() {
			if pending.done != nil {
				close(pending.done)
			}
		})
	} else {
		// A defensive path for shutdown/tests that retired the pending entry
		// before the startup goroutine reported back. Never redirect with an
		// empty stream ID, and always wake any waiter on the old pending.
		if result.err == nil {
			result.err = context.Canceled
		}
		pending.result = result
		pending.doneOnce.Do(func() {
			if pending.done != nil {
				close(pending.done)
			}
		})
	}
	s.hlsMu.Unlock()

	if accepted {
		go s.cleanupHLSStream(pending.streamID, stream)
	}
	return result
}

func (s *Server) startHLSStream(ctx context.Context, vaultID, virtualPath, dir, contentURL, sessionID string) (*hlsStream, error) {
	profiles := buildHLSProfiles()
	var lastErr error
	for profileIndex, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Each profile gets a clean output directory. A timed-out hardware
		// attempt must never leave a stale playlist/segment that makes the next
		// profile look ready.
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("clean hls attempt %d: %w", profileIndex, err)
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("create hls attempt %d: %w", profileIndex, err)
		}
		started := time.Now()
		// The command gets an independent lifetime. The startup context only
		// controls probing/readiness; once ready, stream.cancel owns the process.
		cmd, cancel, stderr, err := startHLSCommand(context.Background(), profile, dir, contentURL, sessionID)
		if err != nil {
			lastErr = err
			continue
		}
		if err := waitForHLSReady(ctx, cmd, dir, 5*time.Second); err != nil {
			cancel()
			if cmd.Process != nil {
				_ = procgroup.Kill(cmd)
			}
			_ = cmd.Wait()
			lastErr = fmt.Errorf("hls %s failed: %w stderr=%s", profile.name, err, trimLog(stderr.String()))
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		log.Printf("hls: started profile=%s in %s", profile.name, time.Since(started).Round(time.Millisecond))
		return &hlsStream{
			dir:         dir,
			cmd:         cmd,
			cancel:      cancel,
			key:         hlsKey{vaultID: vaultID, virtualPath: virtualPath},
			vaultID:     vaultID,
			virtualPath: virtualPath,
			sessionID:   sessionID,
			lastSeen:    time.Now(),
		}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no hls profile available")
}

func startHLSCommand(ctx context.Context, profile transcodeProfile, dir, contentURL, sessionID string) (*exec.Cmd, context.CancelFunc, *tailBuffer, error) {
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
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd)
	stderr := newTailBuffer(hlsMaxStderrBytes)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, nil, err
	}
	cmd.Cancel = func() error {
		cancel()
		return procgroup.Kill(cmd)
	}
	return cmd, cancel, stderr, nil
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		copy(b.buf, b.buf[len(b.buf)-b.limit:])
		b.buf = b.buf[:b.limit]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
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

func (s *Server) handleHLSStop(c *gin.Context) {
	sess := getSession(c)
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Origin, Cookie, X-Session-ID")
	streamID := c.Param("stream")
	rawPath := c.Query("path")
	virtualPath := ""
	if rawPath != "" {
		virtualPath = crypto.NormalizeVirtualPath(rawPath)
	}

	requestSessionID, _ := c.Get("sessionID")
	ownerID, _ := requestSessionID.(string)
	targets := s.collectHLSStopTargetsForOwner(sess.VaultID, ownerID, streamID, virtualPath)
	cancelHLSStopTargets(targets)
	completed := s.waitForHLSStopTargets(c.Request.Context(), targets, 5*time.Second)
	stopped := len(targets.pending) + len(targets.streams)
	if stopped == 0 {
		c.JSON(http.StatusOK, gin.H{"stopped": 0, "complete": true})
		return
	}
	if !completed {
		c.Header("Retry-After", "1")
		c.JSON(http.StatusAccepted, gin.H{
			"stopped":  0,
			"stopping": stopped,
			"complete": false,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"stopped": stopped, "complete": true})
}

type hlsStopTargets struct {
	streams []*hlsStream
	pending []*hlsPending
}

func cancelHLSStopTargets(targets hlsStopTargets) {
	for _, pending := range targets.pending {
		if pending.cancel != nil {
			pending.cancel()
		}
	}
	for _, stream := range targets.streams {
		if stream.cancel != nil {
			stream.cancel()
		}
	}
}

func (s *Server) collectHLSStopTargets(vaultID, streamID, virtualPath string) hlsStopTargets {
	return s.collectHLSStopTargetsForOwner(vaultID, "", streamID, virtualPath)
}

func (s *Server) collectHLSStopTargetsForOwner(vaultID, ownerID, streamID, virtualPath string) hlsStopTargets {
	return s.collectHLSStopTargetsMatching(vaultID, ownerID, streamID, virtualPath, false)
}

// collectHLSStopTargetsForPath includes streams below a directory path. It is
// used by recursive deletion so child videos cannot continue reading while
// their encrypted files are removed.
func (s *Server) collectHLSStopTargetsForPath(vaultID, virtualPath string) hlsStopTargets {
	return s.collectHLSStopTargetsMatching(vaultID, "", "", virtualPath, true)
}

func (s *Server) collectHLSStopTargetsMatching(vaultID, ownerID, streamID, virtualPath string, descendants bool) hlsStopTargets {
	s.hlsMu.Lock()
	defer s.hlsMu.Unlock()

	targets := hlsStopTargets{
		streams: make([]*hlsStream, 0, 1),
		pending: make([]*hlsPending, 0, 1),
	}
	markStream := func(stream *hlsStream) {
		if stream == nil {
			return
		}
		if !stream.stopRequested {
			stream.stopRequested = true
			if stream.stopCh != nil {
				close(stream.stopCh)
			}
		}
		targets.streams = append(targets.streams, stream)
	}
	markPending := func(pending *hlsPending) {
		if pending == nil {
			return
		}
		pending.stopRequested = true
		targets.pending = append(targets.pending, pending)
	}
	releaseOwner := func(owners *map[string]struct{}, refs *map[string]int) bool {
		if ownerID == "" || owners == nil || len(*owners) == 0 {
			return true
		}
		if refs != nil && *refs != nil {
			if count, ok := (*refs)[ownerID]; ok {
				if count > 1 {
					(*refs)[ownerID] = count - 1
					return false
				}
				delete(*refs, ownerID)
				delete(*owners, ownerID)
				return len(*owners) == 0
			}
		}
		if _, ok := (*owners)[ownerID]; !ok {
			return false
		}
		delete(*owners, ownerID)
		return len(*owners) == 0
	}

	if streamID != "" {
		for _, pending := range s.hlsPending {
			if pending.streamID == streamID && pending.key.vaultID == vaultID && releaseOwner(&pending.owners, &pending.ownerRefs) {
				markPending(pending)
			}
		}
		if stream := s.hls[streamID]; stream != nil && stream.vaultID == vaultID && releaseOwner(&stream.owners, &stream.ownerRefs) {
			markStream(stream)
		}
		return targets
	}
	if virtualPath == "" {
		return targets
	}
	key := hlsKey{vaultID: vaultID, virtualPath: virtualPath}
	pathMatches := func(candidate string) bool {
		if !descendants {
			return candidate == virtualPath
		}
		return hlsPathWithin(virtualPath, candidate)
	}
	if !descendants {
		if pending := s.hlsPending[key]; pending != nil && releaseOwner(&pending.owners, &pending.ownerRefs) {
			markPending(pending)
		}
	} else {
		for _, pending := range s.hlsPending {
			if pending != nil && pending.key.vaultID == vaultID && pathMatches(pending.key.virtualPath) && releaseOwner(&pending.owners, &pending.ownerRefs) {
				markPending(pending)
			}
		}
	}
	for _, stream := range s.hls {
		streamPath := stream.virtualPath
		if stream.key != (hlsKey{}) {
			streamPath = stream.key.virtualPath
		}
		matches := stream.vaultID == vaultID && pathMatches(streamPath)
		if matches && releaseOwner(&stream.owners, &stream.ownerRefs) {
			markStream(stream)
		}
	}
	return targets
}

func hlsPathWithin(root, candidate string) bool {
	root = crypto.NormalizeVirtualPath(root)
	candidate = crypto.NormalizeVirtualPath(candidate)
	if root == "/" {
		return true
	}
	return candidate == root || strings.HasPrefix(candidate, strings.TrimSuffix(root, "/")+"/")
}

func addHLSOwner(owners *map[string]struct{}, ownerID string) {
	if owners == nil || ownerID == "" {
		return
	}
	if *owners == nil {
		*owners = make(map[string]struct{})
	}
	(*owners)[ownerID] = struct{}{}
}

func addHLSOwnerRef(owners *map[string]struct{}, refs *map[string]int, ownerID string) {
	if ownerID == "" {
		return
	}
	addHLSOwner(owners, ownerID)
	if refs == nil {
		return
	}
	if *refs == nil {
		*refs = make(map[string]int)
	}
	(*refs)[ownerID]++
}

func cloneHLSOwners(owners map[string]struct{}) map[string]struct{} {
	if len(owners) == 0 {
		return nil
	}
	copyOwners := make(map[string]struct{}, len(owners))
	for ownerID := range owners {
		copyOwners[ownerID] = struct{}{}
	}
	return copyOwners
}

func cloneHLSOwnerRefs(refs map[string]int) map[string]int {
	if len(refs) == 0 {
		return nil
	}
	copyRefs := make(map[string]int, len(refs))
	for ownerID, count := range refs {
		copyRefs[ownerID] = count
	}
	return copyRefs
}

func hlsOwnerAllowed(owners map[string]struct{}, ownerID string) bool {
	// Streams created by older in-memory state/tests have no owner set; the
	// vault check remains the compatibility fallback for those entries.
	if len(owners) == 0 {
		return true
	}
	_, ok := owners[ownerID]
	return ok
}

func (s *Server) collectAllHLSStopTargets(vaultID string) hlsStopTargets {
	s.hlsMu.Lock()
	defer s.hlsMu.Unlock()

	targets := hlsStopTargets{
		streams: make([]*hlsStream, 0, 1),
		pending: make([]*hlsPending, 0, 1),
	}
	for _, pending := range s.hlsPending {
		if pending == nil || (vaultID != "" && pending.key.vaultID != vaultID) {
			continue
		}
		pending.stopRequested = true
		targets.pending = append(targets.pending, pending)
	}
	for _, stream := range s.hls {
		if stream == nil || (vaultID != "" && stream.vaultID != vaultID) {
			continue
		}
		if !stream.stopRequested {
			stream.stopRequested = true
			if stream.stopCh != nil {
				close(stream.stopCh)
			}
		}
		targets.streams = append(targets.streams, stream)
	}
	return targets
}

// Shutdown stops every pending and active HLS stream and waits for its
// cleanup goroutine. It is intended to run before the HTTP server and session
// store are closed, so FFmpeg cannot outlive the main process during a
// graceful shutdown. The caller controls the upper bound through ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.hlsMu.Lock()
	s.hlsClosing = true
	s.hlsMu.Unlock()
	targets := s.collectAllHLSStopTargets("")
	cancelHLSStopTargets(targets)
	if waitForHLSStopTargetsContext(ctx, targets) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.DeadlineExceeded
}

func (s *Server) collectHLSOwnerTargets(vaultID, ownerID string) hlsStopTargets {
	s.hlsMu.Lock()
	defer s.hlsMu.Unlock()

	targets := hlsStopTargets{
		streams: make([]*hlsStream, 0, 1),
		pending: make([]*hlsPending, 0, 1),
	}
	for _, pending := range s.hlsPending {
		if pending == nil || pending.key.vaultID != vaultID {
			continue
		}
		if len(pending.owners) > 0 {
			if _, ok := pending.owners[ownerID]; !ok {
				continue
			}
			delete(pending.owners, ownerID)
			if pending.ownerRefs != nil {
				delete(pending.ownerRefs, ownerID)
			}
			if len(pending.owners) > 0 {
				continue
			}
		}
		pending.stopRequested = true
		targets.pending = append(targets.pending, pending)
	}
	for _, stream := range s.hls {
		if stream == nil || stream.vaultID != vaultID {
			continue
		}
		if len(stream.owners) > 0 {
			if _, ok := stream.owners[ownerID]; !ok {
				continue
			}
			delete(stream.owners, ownerID)
			if stream.ownerRefs != nil {
				delete(stream.ownerRefs, ownerID)
			}
			if len(stream.owners) > 0 {
				continue
			}
		}
		if !stream.stopRequested {
			stream.stopRequested = true
			if stream.stopCh != nil {
				close(stream.stopCh)
			}
		}
		targets.streams = append(targets.streams, stream)
	}
	return targets
}

func (s *Server) waitForHLSStopTargets(ctx context.Context, targets hlsStopTargets, timeout time.Duration) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return waitForHLSStopTargetsContext(timeoutCtx, targets)
}

func waitForHLSStopTargetsContext(ctx context.Context, targets hlsStopTargets) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	wait := func(done <-chan struct{}) bool {
		if done == nil {
			return true
		}
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for _, pending := range targets.pending {
		if !wait(pending.done) {
			return false
		}
	}
	for _, stream := range targets.streams {
		if !wait(stream.doneCh) {
			return false
		}
	}
	return true
}

// stopHLSStreams is kept as a small compatibility wrapper for callers/tests
// that only need the already-running streams.
func (s *Server) stopHLSStreams(vaultID, streamID, virtualPath string) []*hlsStream {
	return s.collectHLSStopTargets(vaultID, streamID, virtualPath).streams
}

// stopHLSForVault is used before destructive vault operations. It marks and
// cancels every pending/active stream, then gives the cleanup goroutines a
// bounded window to reap FFmpeg and remove their temporary directories. A
// false result means callers must not proceed with a destructive operation.
func (s *Server) stopHLSForVault(vaultID string) bool {
	targets := s.collectAllHLSStopTargets(vaultID)
	cancelHLSStopTargets(targets)
	return s.waitForHLSStopTargets(context.Background(), targets, 5*time.Second)
}

func (s *Server) stopHLSForPath(vaultID, virtualPath string) bool {
	if virtualPath == "" {
		return true
	}
	targets := s.collectHLSStopTargetsForPath(vaultID, crypto.NormalizeVirtualPath(virtualPath))
	cancelHLSStopTargets(targets)
	return s.waitForHLSStopTargets(context.Background(), targets, 5*time.Second)
}

// PrepareFileReplacement stops any HLS stream that may still be reading the
// old contents of a path. It is exported for the background import manager,
// which otherwise has no safe way to coordinate an overwrite.
func (s *Server) PrepareFileReplacement(vaultID, virtualPath string) error {
	release, err := s.BeginFileReplacement(vaultID, virtualPath)
	if release != nil {
		release()
	}
	return err
}

// BeginFileReplacement acquires the replacement barrier used by background
// imports. The returned release function must be called after the destination
// file has been written; until then new HLS starts for any path are blocked.
func (s *Server) BeginFileReplacement(vaultID, virtualPath string) (func(), error) {
	s.hlsLifeMu.Lock()
	if !s.stopHLSForPath(vaultID, virtualPath) {
		s.hlsLifeMu.Unlock()
		return nil, fmt.Errorf("active video stream is still stopping")
	}
	var once sync.Once
	return func() {
		once.Do(func() { s.hlsLifeMu.Unlock() })
	}, nil
}

func (s *Server) stopHLSForOwner(vaultID, ownerID string) bool {
	if ownerID == "" {
		return true
	}
	targets := s.collectHLSOwnerTargets(vaultID, ownerID)
	cancelHLSStopTargets(targets)
	return s.waitForHLSStopTargets(context.Background(), targets, 5*time.Second)
}

func stopAndWaitHLS(stream *hlsStream) {
	if stream == nil {
		return
	}
	if stream.cancel != nil {
		stream.cancel()
	}
	if stream.cmd == nil {
		return
	}
	// Signal unconditionally when a process handle exists. ProcessState is
	// mutated by Wait, so reading it here would race with cleanup's waiter;
	// an already-exited process simply makes Kill return an ignorable error.
	if stream.cmd.Process != nil {
		_ = procgroup.Kill(stream.cmd)
	}
	_ = waitHLSCommand(stream)
}

// waitHLSCommand serializes calls to exec.Cmd.Wait.  A stream can be stopped
// while its cleanup goroutine is already waiting for the same process; calling
// Wait twice is invalid and can lose the real process error or panic in tests.
func waitHLSCommand(stream *hlsStream) error {
	if stream == nil || stream.cmd == nil {
		return nil
	}
	stream.waitOnce.Do(func() {
		stream.waitErr = stream.cmd.Wait()
	})
	return stream.waitErr
}

func (s *Server) cleanupHLSStream(streamID string, stream *hlsStream) {
	if stream == nil {
		return
	}
	if stream.doneCh != nil {
		defer stream.doneOnce.Do(func() { close(stream.doneCh) })
	}
	var err error
	done := make(chan error, 1)
	if stream.cmd != nil {
		go func() { done <- waitHLSCommand(stream) }()
	} else {
		done <- nil
	}
	idleTicker := time.NewTicker(hlsIdleCheckInterval)
	defer idleTicker.Stop()
	// The process is allowed to finish naturally for VOD streams. While it is
	// running, periodically stop streams that no longer receive asset requests.
	for {
		select {
		case err = <-done:
			goto finished
		case <-idleTicker.C:
			shouldStop := false
			s.hlsMu.Lock()
			if !stream.stopRequested && time.Since(stream.lastSeen) > hlsIdleTimeout {
				stream.stopRequested = true
				if stream.stopCh != nil {
					close(stream.stopCh)
				}
				shouldStop = true
			}
			s.hlsMu.Unlock()
			if shouldStop && stream.cancel != nil {
				stream.cancel()
			}
		}
	}

finished:
	playlistComplete := err == nil && hlsPlaylistComplete(stream.dir)
	s.hlsMu.Lock()
	stoppedByRequest := stream.stopRequested
	if !stream.finished {
		stream.finished = true
		if s.hlsActive > 0 {
			s.hlsActive--
		}
	}
	if stream.stopCh == nil {
		stream.stopCh = make(chan struct{})
	}
	removeNow := stoppedByRequest || err != nil || !playlistComplete
	if removeNow && s.hls[streamID] == stream {
		delete(s.hls, streamID)
	}
	s.hlsMu.Unlock()
	if !stoppedByRequest {
		if err != nil {
			log.Printf("hls: stream %s ended: %v", streamID, err)
		} else if !playlistComplete {
			log.Printf("hls: stream %s ended without a complete playlist", streamID)
		}
	}
	if s.sessions != nil {
		s.sessions.Delete(stream.sessionID)
	}

	if removeNow {
		removeHLSDir(stream)
		return
	}

	retention := time.NewTimer(hlsFinishedRetention)
	select {
	case <-retention.C:
	case <-stream.stopCh:
		if !retention.Stop() {
			select {
			case <-retention.C:
			default:
			}
		}
	}
	s.hlsMu.Lock()
	if s.hls[streamID] == stream {
		delete(s.hls, streamID)
	}
	s.hlsMu.Unlock()
	removeHLSDir(stream)
}

func removeHLSDir(stream *hlsStream) {
	if stream == nil {
		return
	}
	stream.assetMu.Lock()
	defer stream.assetMu.Unlock()
	_ = os.RemoveAll(stream.dir)
}

func hlsPlaylistComplete(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	return err == nil && strings.Contains(string(data), "#EXT-X-ENDLIST")
}

func (s *Server) handleHLSAsset(c *gin.Context) {
	sess := getSession(c)
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Origin, Cookie, X-Session-ID")
	requestSessionID, _ := c.Get("sessionID")
	ownerID, _ := requestSessionID.(string)
	streamID := c.Param("stream")
	name := filepath.Base(c.Param("name"))
	if name == "." || name == string(filepath.Separator) || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid hls asset"})
		return
	}

	s.hlsMu.Lock()
	stream := s.hls[streamID]
	var stopCh <-chan struct{}
	if stream != nil && stream.vaultID == sess.VaultID && !stream.stopRequested && hlsOwnerAllowed(stream.owners, ownerID) {
		stream.lastSeen = time.Now()
		stopCh = stream.stopCh
	} else if stream != nil {
		stream = nil
	}
	s.hlsMu.Unlock()
	if stream == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "hls stream not found"})
		return
	}
	// Cleanup takes the write side of assetMu before removing the stream
	// directory. Holding a read lock through c.File closes the existence/open
	// TOCTOU window between waitForHLSFile and serving the asset.
	stream.assetMu.RLock()
	defer stream.assetMu.RUnlock()

	path := filepath.Join(stream.dir, name)
	if name == "index.m3u8" {
		c.Header("Content-Type", "application/vnd.apple.mpegurl")
		if err := waitForHLSFile(c.Request.Context(), stopCh, path, 30*time.Second); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "hls playlist not ready"})
			return
		}
		c.File(path)
		return
	}
	if strings.HasSuffix(name, ".ts") {
		if err := waitForHLSFile(c.Request.Context(), stopCh, path, 30*time.Second); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "hls segment not ready"})
			return
		}
		c.Header("Content-Type", "video/mp2t")
		c.Header("Cache-Control", "private, max-age=30")
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

func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	return waitForHLSFile(ctx, nil, path, timeout)
}

func waitForHLSFile(ctx context.Context, stopCh <-chan struct{}, path string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopCh:
			return context.Canceled
		default:
		}
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopCh:
			return context.Canceled
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
		device := findHLSVAAPIDevice()
		if device == "" {
			return transcodeProfile{}, false
		}
		return transcodeProfile{
			name: "vaapi-hwdec",
			beforeInputArgs: []string{
				"-hwaccel", "vaapi",
				"-hwaccel_device", device,
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
		device := findHLSVAAPIDevice()
		if device == "" {
			return transcodeProfile{}, false
		}
		return transcodeProfile{
			name:            "vaapi",
			beforeInputArgs: []string{"-vaapi_device", device},
			videoArgs:       []string{"-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-qp", "24"},
		}, true
	default:
		return transcodeProfile{}, false
	}
}

func findHLSVAAPIDevice() string {
	devices, err := filepath.Glob("/dev/dri/renderD*")
	if err != nil || len(devices) == 0 {
		return ""
	}
	sort.Strings(devices)
	for _, device := range devices {
		info, statErr := os.Stat(device)
		if statErr != nil || info.Mode()&os.ModeCharDevice == 0 {
			continue
		}
		fd, openErr := os.OpenFile(device, os.O_RDWR, 0)
		if openErr != nil {
			continue
		}
		_ = fd.Close()
		return device
	}
	return ""
}

func cpuTranscodeProfile() transcodeProfile {
	return transcodeProfile{
		name:      "cpu",
		videoArgs: []string{"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p"},
	}
}

func detectFFmpegEncoders() map[string]bool {
	// Serialize probes so a temporary ffmpeg/driver outage cannot launch one
	// five-second process per concurrent playback request. Successful results
	// are cached briefly; failures deliberately remain uncached and can be
	// retried when the binary or device becomes available again.
	ffmpegEncodersMu.Lock()
	defer ffmpegEncodersMu.Unlock()
	now := time.Now()
	if ffmpegEncodersCacheValid && now.Sub(ffmpegEncodersCachedAt) < ffmpegEncodersCacheTTL {
		return cloneFFmpegEncoders(ffmpegEncodersCache)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ffmpegDetectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders")
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		ffmpegEncodersCache = nil
		ffmpegEncodersCacheValid = false
		ffmpegEncodersCachedAt = time.Time{}
		return map[string]bool{}
	}

	encoders := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			encoders[fields[1]] = true
		}
	}
	ffmpegEncodersCache = encoders
	ffmpegEncodersCachedAt = now
	ffmpegEncodersCacheValid = true
	return cloneFFmpegEncoders(encoders)
}

func cloneFFmpegEncoders(encoders map[string]bool) map[string]bool {
	copyEncoders := make(map[string]bool, len(encoders))
	for name, available := range encoders {
		copyEncoders[name] = available
	}
	return copyEncoders
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
	// Keep new HLS starts out of the stop-and-replace window. The write lock is
	// held through encryption and indexing, so a concurrent player cannot
	// start reading the destination again before it is complete.
	s.hlsLifeMu.Lock()
	defer s.hlsLifeMu.Unlock()
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve path: " + err.Error()})
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
		// The encrypted destination may have replaced an older file at the same
		// virtual path. Invalidate its cached preview before enqueueing; Enqueue
		// intentionally skips existing thumbnails to deduplicate work.
		s.thumbs.DeleteThumbnail(sess.VaultID, virtualPath)
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
	s.hlsLifeMu.Lock()
	defer s.hlsLifeMu.Unlock()

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
		// Thumbnail not yet generated — trigger async generation and return 404
		s.thumbs.Enqueue(sess.VaultID, sess.VaultPath, sess.Keys, path)
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
