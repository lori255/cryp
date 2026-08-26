package api

// This file owns HLS admission, FFmpeg process lifecycle, profile selection,
// asset serving, and shutdown coordination. It intentionally stays in the api
// package so existing Server state and route symbols remain unchanged.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
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

	"github.com/gin-gonic/gin"
)

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
	streamID        string
	durationSeconds float64
	err             error
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
	dir    string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	// processWait owns the command's single Wait call. Readiness and cleanup
	// both observe the same exit notification, so an early FFmpeg failure is
	// detected immediately without introducing a second Wait race.
	processWait   *hlsProcessWait
	key           hlsKey
	vaultID       string
	virtualPath   string
	sessionID     string
	lastSeen      time.Time
	stopRequested bool
	finished      bool
	// durationSeconds is the stable source duration exposed to the player while
	// the FFmpeg playlist is still growing. The remaining fields are retained
	// only for older in-package tests; playlists are served directly by FFmpeg.
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
	ffmpegEncodersMu         sync.Mutex
	ffmpegEncodersCache      map[string]bool
	ffmpegEncodersCachedAt   time.Time
	ffmpegEncodersCacheValid bool
	ffmpegEncodersCacheBin   string
	hlsProfileFailuresMu     sync.Mutex
	hlsProfileFailures       map[string]time.Time
	hlsProcessWaiters        sync.Map // map[*exec.Cmd]*hlsProcessWait
)

const (
	ffmpegEncodersCacheTTL = time.Minute
	hlsProfileFailureTTL   = 30 * time.Second
)

func ffmpegBinary() string {
	if bin := strings.TrimSpace(os.Getenv("CRYP_FFMPEG_BIN")); bin != "" {
		return bin
	}
	return "ffmpeg"
}

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
		if !s.requestSessionActive(c) {
			s.hlsLifeMu.RUnlock()
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
			return
		}
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
				redirectHLSStream(c, sess.VaultID, existingID, existing.durationSeconds)
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
			c.JSON(hlsStartHTTPStatus(errHLSCapacity), gin.H{
				"error": "too many active video streams",
				"code":  hlsStartErrorCode(errHLSCapacity),
			})
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
	duration, err := s.storedMediaDuration(keysCopy, pending.key)
	if err != nil {
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
	stream.durationSeconds = duration

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
	if keys != nil {
		keys.Zero()
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

func validMediaDuration(duration float64) bool {
	return !math.IsNaN(duration) && !math.IsInf(duration, 0) && duration > 0
}

func redirectHLSStream(c *gin.Context, vaultID, streamID string, durationSeconds float64) {
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Origin, Cookie, X-Session-ID")
	location := fmt.Sprintf("/api/vaults/%s/files/hls/%s/index.m3u8", vaultID, streamID)
	if validMediaDuration(durationSeconds) {
		location += "?duration=" + strconv.FormatFloat(durationSeconds, 'f', 3, 64)
	}
	c.Redirect(http.StatusFound, location)
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
			c.JSON(hlsStartHTTPStatus(result.err), gin.H{
				"error": "failed to start hls transcode",
				"code":  hlsStartErrorCode(result.err),
			})
			return
		}
		// The request can be cancelled in the tiny interval between the select
		// wake-up and writing the redirect. Re-check before attaching the caller
		// to the stream so a disconnect cannot strand its last owner.
		if c.Request.Context().Err() != nil {
			s.releaseHLSStartOwner(vaultID, ownerID, result.streamID)
			return
		}
		redirectHLSStream(c, vaultID, result.streamID, result.durationSeconds)
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
			result.durationSeconds = stream.durationSeconds
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
		if hlsProfileCoolingDown(profile) {
			log.Printf("hls: skipping profile=%s during backend failure cooldown", profile.name)
			continue
		}
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
			markHLSProfileFailure(profile)
			lastErr = err
			continue
		}
		processWait := lookupHLSProcessWait(cmd)
		if err := waitForHLSReadyWithProcess(ctx, cmd, dir, 5*time.Second, processWait); err != nil {
			cancel()
			killHLSProcess(cmd, processWait)
			_ = waitHLSProcess(cmd, processWait)
			forgetHLSProcessWait(cmd, processWait)
			lastErr = fmt.Errorf("hls %s failed: %w stderr=%s", profile.name, err, trimLog(stderr.String()))
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			markHLSProfileFailure(profile)
			continue
		}
		clearHLSProfileFailure(profile)
		log.Printf("hls: started profile=%s in %s", profile.name, time.Since(started).Round(time.Millisecond))
		stream := &hlsStream{
			dir:         dir,
			cmd:         cmd,
			cancel:      cancel,
			processWait: processWait,
			key:         hlsKey{vaultID: vaultID, virtualPath: virtualPath},
			vaultID:     vaultID,
			virtualPath: virtualPath,
			sessionID:   sessionID,
			lastSeen:    time.Now(),
		}
		// The stream now owns the wait state; remove the compatibility lookup
		// entry so completed starts cannot accumulate in the global registry.
		forgetHLSProcessWait(cmd, processWait)
		return stream, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no hls profile available")
}

func hlsProfileKey(profile transcodeProfile) string {
	// CPU is the stable fallback and should never be cooled down. Include the
	// complete argument shape so a device/profile change is allowed to recover
	// immediately instead of inheriting a stale failure for the old setup.
	if profile.name == "" || profile.name == "cpu" {
		return ""
	}
	return ffmpegBinary() + "\x00" + profile.name + "\x00" +
		strings.Join(profile.beforeInputArgs, "\x00") + "\x00" +
		strings.Join(profile.videoArgs, "\x00")
}

func hlsProfileCoolingDown(profile transcodeProfile) bool {
	key := hlsProfileKey(profile)
	if key == "" {
		return false
	}
	hlsProfileFailuresMu.Lock()
	defer hlsProfileFailuresMu.Unlock()
	if hlsProfileFailures == nil {
		return false
	}
	until, ok := hlsProfileFailures[key]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(hlsProfileFailures, key)
	return false
}

func markHLSProfileFailure(profile transcodeProfile) {
	key := hlsProfileKey(profile)
	if key == "" {
		return
	}
	hlsProfileFailuresMu.Lock()
	if hlsProfileFailures == nil {
		hlsProfileFailures = make(map[string]time.Time)
	}
	hlsProfileFailures[key] = time.Now().Add(hlsProfileFailureTTL)
	hlsProfileFailuresMu.Unlock()
}

func clearHLSProfileFailure(profile transcodeProfile) {
	key := hlsProfileKey(profile)
	if key == "" {
		return
	}
	hlsProfileFailuresMu.Lock()
	delete(hlsProfileFailures, key)
	hlsProfileFailuresMu.Unlock()
}

func startHLSCommand(ctx context.Context, profile transcodeProfile, dir, contentURL, sessionID string) (*exec.Cmd, context.CancelFunc, *procgroup.TailBuffer, error) {
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
	cmd := exec.CommandContext(streamCtx, ffmpegBinary(), args...)
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd)
	stderr := procgroup.NewTailBuffer(hlsMaxStderrBytes)
	cmd.Stderr = stderr
	cmd.Cancel = func() error {
		cancel()
		return procgroup.Kill(cmd)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, nil, err
	}
	registerHLSProcessWait(cmd)
	return cmd, cancel, stderr, nil
}

// hlsProcessWait owns the only call to exec.Cmd.Wait for an HLS process. The
// waiter starts lazily so legacy in-package callers that only start a command
// retain the same setup semantics.
type hlsProcessWait struct {
	startOnce sync.Once
	done      chan struct{}
	err       error
}

func newHLSProcessWait() *hlsProcessWait {
	return &hlsProcessWait{done: make(chan struct{})}
}

func (w *hlsProcessWait) start(cmd *exec.Cmd) {
	if w == nil || cmd == nil {
		return
	}
	w.startOnce.Do(func() {
		go func() {
			w.err = cmd.Wait()
			close(w.done)
		}()
	})
}

func (w *hlsProcessWait) wait(cmd *exec.Cmd) error {
	if w == nil {
		if cmd == nil {
			return nil
		}
		return cmd.Wait()
	}
	w.start(cmd)
	<-w.done
	return w.err
}

func registerHLSProcessWait(cmd *exec.Cmd) *hlsProcessWait {
	if cmd == nil {
		return nil
	}
	w := newHLSProcessWait()
	hlsProcessWaiters.Store(cmd, w)
	return w
}

func lookupHLSProcessWait(cmd *exec.Cmd) *hlsProcessWait {
	if cmd == nil {
		return nil
	}
	if value, ok := hlsProcessWaiters.Load(cmd); ok {
		if w, ok := value.(*hlsProcessWait); ok {
			return w
		}
	}
	return nil
}

func forgetHLSProcessWait(cmd *exec.Cmd, want *hlsProcessWait) {
	if cmd == nil || want == nil {
		return
	}
	hlsProcessWaiters.CompareAndDelete(cmd, want)
}

func waitHLSProcess(cmd *exec.Cmd, processWait *hlsProcessWait) error {
	if cmd == nil {
		return nil
	}
	if processWait == nil {
		processWait = lookupHLSProcessWait(cmd)
	}
	if processWait != nil {
		return processWait.wait(cmd)
	}
	return cmd.Wait()
}

func killHLSProcess(cmd *exec.Cmd, processWait *hlsProcessWait) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if processWait != nil {
		select {
		case <-processWait.done:
			// The waiter has already reaped the child; sending a signal to a
			// potentially reused PID would be unsafe and unnecessary.
			return
		default:
		}
	}
	_ = procgroup.Kill(cmd)
}

func waitForHLSReady(ctx context.Context, cmd *exec.Cmd, dir string, timeout time.Duration) error {
	return waitForHLSReadyWithProcess(ctx, cmd, dir, timeout, lookupHLSProcessWait(cmd))
}

func waitForHLSReadyWithProcess(ctx context.Context, cmd *exec.Cmd, dir string, timeout time.Duration, processWait *hlsProcessWait) error {
	if ctx == nil {
		ctx = context.Background()
	}
	playlistPath := filepath.Join(dir, "index.m3u8")
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	if processWait != nil {
		processWait.start(cmd)
	}
	var processDone <-chan struct{}
	if processWait != nil {
		processDone = processWait.done
	}
	playlistReady := func() bool {
		data, err := os.ReadFile(playlistPath)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasSuffix(line, ".ts") {
				if _, statErr := os.Stat(filepath.Join(dir, filepath.Base(line))); statErr == nil {
					return true
				}
			}
		}
		return false
	}

	for {
		if playlistReady() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("playlist timeout")
		case <-processDone:
			// A process can finish just after publishing the first segment.
			// Re-check files before reporting an early failure.
			if playlistReady() {
				return nil
			}
			if processWait != nil && processWait.err != nil {
				return fmt.Errorf("ffmpeg exited: %w", processWait.err)
			}
			return fmt.Errorf("ffmpeg exited")
		case <-ticker.C:
			// Compatibility path for commands created by older in-package
			// callers that were not registered through startHLSCommand. Such
			// callers retain ownership of Wait, so only inspect ProcessState.
			if processWait == nil && cmd != nil && cmd.ProcessState != nil && cmd.ProcessState.Exited() {
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
// cleanup goroutine. It first takes the lifecycle write barrier so an upload,
// import replacement, or destructive file operation cannot continue while
// sessions and the vault filesystem are being torn down. It is intended to
// run before the HTTP server and session store are closed; the caller controls
// the upper bound through ctx for process-level graceful shutdown.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// HTTP shutdown stops admission before this method is called in production.
	// Holding the barrier here also drains any already-running mutation before
	// we close its dependent session/DB resources.
	if err := s.acquireHLSLifecycle(ctx); err != nil {
		return err
	}
	defer s.hlsLifeMu.Unlock()
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

// acquireHLSLifecycle is a context-aware write-lock acquisition used only by
// shutdown. The ordinary request paths use the mutex directly because they
// must hold the barrier across a filesystem operation; shutdown additionally
// needs to honor its grace-period budget while waiting for one such operation
// to finish.
func (s *Server) acquireHLSLifecycle(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.hlsLifeMu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
	// Signal while the process is still live. The shared wait state lets us
	// avoid touching a handle that another goroutine has already reaped.
	killHLSProcess(stream.cmd, stream.processWait)
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
		if stream.processWait == nil {
			stream.processWait = lookupHLSProcessWait(stream.cmd)
		}
		stream.waitErr = waitHLSProcess(stream.cmd, stream.processWait)
		forgetHLSProcessWait(stream.cmd, stream.processWait)
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

func parseFFmpegEncoderList(r io.Reader) (map[string]bool, error) {
	encoders := make(map[string]bool)
	scanner := bufio.NewScanner(r)
	// FFmpeg emits one encoder per short line. Bound a malformed line while
	// continuing to stream arbitrarily long, valid listings without retaining
	// the complete command output.
	scanner.Buffer(make([]byte, 1024), 64<<10)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[1] == "h264_vaapi" {
			encoders[fields[1]] = true
		}
	}
	return encoders, scanner.Err()
}

func runFFmpegEncoderProbe(ctx context.Context, bin string) (map[string]bool, error) {
	cmd := exec.CommandContext(ctx, bin, "-hide_banner", "-encoders")
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := procgroup.NewTailBuffer(hlsMaxStderrBytes)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	encoders, scanErr := parseFFmpegEncoderList(stdout)
	waitErr := cmd.Wait()
	if scanErr != nil {
		return nil, scanErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("ffmpeg encoder probe: %w: %s", waitErr, trimLog(stderr.String()))
	}
	return encoders, nil
}

func detectFFmpegEncoders() map[string]bool {
	// Serialize probes so a temporary ffmpeg/driver outage cannot launch one
	// five-second process per concurrent playback request. Successful results
	// are cached briefly; failures deliberately remain uncached and can be
	// retried when the binary or device becomes available again.
	ffmpegEncodersMu.Lock()
	defer ffmpegEncodersMu.Unlock()
	now := time.Now()
	bin := ffmpegBinary()
	if ffmpegEncodersCacheValid && ffmpegEncodersCacheBin == bin && now.Sub(ffmpegEncodersCachedAt) < ffmpegEncodersCacheTTL {
		return cloneFFmpegEncoders(ffmpegEncodersCache)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ffmpegDetectTimeout)
	defer cancel()
	encoders, err := runFFmpegEncoderProbe(ctx, bin)
	if err != nil {
		ffmpegEncodersCache = nil
		ffmpegEncodersCacheValid = false
		ffmpegEncodersCachedAt = time.Time{}
		ffmpegEncodersCacheBin = bin
		return map[string]bool{}
	}

	ffmpegEncodersCache = encoders
	ffmpegEncodersCachedAt = now
	ffmpegEncodersCacheValid = true
	ffmpegEncodersCacheBin = bin
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
