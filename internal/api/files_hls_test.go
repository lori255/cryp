package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestMain doubles as a tiny fake-FFmpeg executable for the lifecycle test
// below. It is activated only in a child process through an environment flag,
// so normal package tests retain their usual runner.
func TestMain(m *testing.M) {
	if os.Getenv("CRYP_FAKE_FFMPEG_HELPER") == "1" {
		playlist := os.Args[len(os.Args)-1]
		if strings.HasPrefix(playlist, "-") {
			os.Exit(0)
		}
		dir := filepath.Dir(playlist)
		_ = os.MkdirAll(dir, 0o700)
		const segment = "segment_00000.ts"
		_ = os.WriteFile(filepath.Join(dir, segment), []byte("fake segment"), 0o600)
		_ = os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:1,\n"+segment+"\n"), 0o600)
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(m.Run())
}

func TestCompleteHLSStartBroadcastsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pending := &hlsPending{
		key:      hlsKey{vaultID: "vault", virtualPath: "/video.mp4"},
		streamID: "stream",
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	s := &Server{
		hls:        make(map[string]*hlsStream),
		hlsPending: map[hlsKey]*hlsPending{pending.key: pending},
		hlsStarts:  1,
	}
	wantErr := errors.New("startup failed")
	result := s.completeHLSStart(pending, nil, wantErr)
	if !errors.Is(result.err, wantErr) {
		t.Fatalf("owner result error = %v, want %v", result.err, wantErr)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-pending.done:
			if !errors.Is(pending.result.err, wantErr) {
				t.Fatalf("waiter result error = %v, want %v", pending.result.err, wantErr)
			}
		case <-time.After(time.Second):
			t.Fatal("pending start was not completed")
		}
	}
	if s.hlsStarts != 0 {
		t.Fatalf("hlsStarts = %d, want 0", s.hlsStarts)
	}
}

func TestCompleteHLSStartRetiredPendingReturnsCancellation(t *testing.T) {
	key := hlsKey{vaultID: "vault", virtualPath: "/video.mp4"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := &hlsPending{key: key, streamID: "old", ctx: ctx, done: make(chan struct{})}
	other := &hlsPending{key: key, streamID: "new", ctx: ctx, done: make(chan struct{})}
	s := &Server{
		hls:        make(map[string]*hlsStream),
		hlsPending: map[hlsKey]*hlsPending{key: other},
	}
	result := s.completeHLSStart(old, nil, nil)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("retired pending result = %v, want cancellation", result.err)
	}
	select {
	case <-old.done:
	default:
		t.Fatal("retired pending waiters were not released")
	}
}

func TestCollectHLSStopTargetsPendingAndRunning(t *testing.T) {
	key := hlsKey{vaultID: "vault", virtualPath: "/video.mp4"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pending := &hlsPending{
		key:      key,
		streamID: "pending-stream",
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	stream := &hlsStream{
		key:           key,
		vaultID:       key.vaultID,
		virtualPath:   key.virtualPath,
		stopCh:        make(chan struct{}),
		stopRequested: false,
	}
	s := &Server{
		hls:        map[string]*hlsStream{"running-stream": stream},
		hlsPending: map[hlsKey]*hlsPending{key: pending},
	}

	targets := s.collectHLSStopTargets(key.vaultID, "", key.virtualPath)
	if len(targets.pending) != 1 || len(targets.streams) != 1 {
		t.Fatalf("stop targets = pending %d, streams %d; want 1/1", len(targets.pending), len(targets.streams))
	}
	if !pending.stopRequested || !stream.stopRequested {
		t.Fatal("stop request was not recorded")
	}
	select {
	case <-stream.stopCh:
	default:
		t.Fatal("stream stop signal was not closed")
	}

	second := s.collectHLSStopTargets(key.vaultID, "", key.virtualPath)
	if len(second.pending) != 1 || len(second.streams) != 1 {
		t.Fatalf("repeated stop returned pending %d, streams %d; want 1/1 so callers can await cleanup", len(second.pending), len(second.streams))
	}
}

func TestCollectHLSStopTargetsForPathIncludesDescendants(t *testing.T) {
	parent := hlsKey{vaultID: "vault", virtualPath: "/movies"}
	child := hlsKey{vaultID: "vault", virtualPath: "/movies/clip.mp4"}
	other := hlsKey{vaultID: "vault", virtualPath: "/music/clip.mp4"}
	s := &Server{
		hls: map[string]*hlsStream{
			"parent": {key: parent, vaultID: parent.vaultID, virtualPath: parent.virtualPath, stopCh: make(chan struct{})},
			"child":  {key: child, vaultID: child.vaultID, virtualPath: child.virtualPath, stopCh: make(chan struct{})},
			"other":  {key: other, vaultID: other.vaultID, virtualPath: other.virtualPath, stopCh: make(chan struct{})},
		},
		hlsPending: make(map[hlsKey]*hlsPending),
	}
	targets := s.collectHLSStopTargetsForPath("vault", "/movies")
	if len(targets.streams) != 2 {
		t.Fatalf("descendant stop targets = %d, want 2", len(targets.streams))
	}
	if !s.hls["parent"].stopRequested || !s.hls["child"].stopRequested {
		t.Fatal("parent/child streams were not marked for stop")
	}
	if s.hls["other"].stopRequested {
		t.Fatal("unrelated stream was marked for stop")
	}
}

func TestHLSOwnerReleaseKeepsSharedStreamRunning(t *testing.T) {
	key := hlsKey{vaultID: "vault", virtualPath: "/video.mp4"}
	stream := &hlsStream{
		key:         key,
		vaultID:     key.vaultID,
		virtualPath: key.virtualPath,
		stopCh:      make(chan struct{}),
		owners:      map[string]struct{}{"one": {}, "two": {}},
	}
	s := &Server{hls: map[string]*hlsStream{"stream": stream}, hlsPending: make(map[hlsKey]*hlsPending)}
	targets := s.collectHLSStopTargetsForOwner(key.vaultID, "one", "stream", "")
	if len(targets.streams) != 0 {
		t.Fatal("shared stream was stopped when one owner left")
	}
	if stream.stopRequested {
		t.Fatal("shared stream was marked stopped")
	}
	if _, ok := stream.owners["one"]; ok {
		t.Fatal("released owner remained attached")
	}

	targets = s.collectHLSStopTargetsForOwner(key.vaultID, "two", "stream", "")
	if len(targets.streams) != 1 || !stream.stopRequested {
		t.Fatal("last owner did not stop stream")
	}
}

func TestHLSOwnerReferencesKeepSameSessionStreamRunning(t *testing.T) {
	key := hlsKey{vaultID: "vault", virtualPath: "/video.mp4"}
	stream := &hlsStream{
		key:         key,
		vaultID:     key.vaultID,
		virtualPath: key.virtualPath,
		stopCh:      make(chan struct{}),
		owners:      map[string]struct{}{"session": {}},
		ownerRefs:   map[string]int{"session": 2},
	}
	s := &Server{hls: map[string]*hlsStream{"stream": stream}, hlsPending: make(map[hlsKey]*hlsPending)}

	first := s.collectHLSStopTargetsForOwner(key.vaultID, "session", "stream", "")
	if len(first.streams) != 0 || stream.stopRequested {
		t.Fatal("first same-session release stopped a stream with another active reference")
	}
	if stream.ownerRefs["session"] != 1 {
		t.Fatalf("owner reference count = %d, want 1", stream.ownerRefs["session"])
	}

	second := s.collectHLSStopTargetsForOwner(key.vaultID, "session", "stream", "")
	if len(second.streams) != 1 || !stream.stopRequested {
		t.Fatal("last same-session release did not stop the stream")
	}
}

func TestWaitForHLSStartOwnerCancellationReleasesOneReference(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	req := httptest.NewRequest("GET", "/api/vaults/vault/files/hls", nil).WithContext(requestCtx)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	startCtx, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	pending := &hlsPending{
		key:       hlsKey{vaultID: "vault", virtualPath: "/video.mp4"},
		streamID:  "pending",
		ctx:       startCtx,
		cancel:    cancelStart,
		done:      make(chan struct{}),
		owners:    map[string]struct{}{"session": {}},
		ownerRefs: map[string]int{"session": 2},
	}
	s := &Server{
		hls:        make(map[string]*hlsStream),
		hlsPending: map[hlsKey]*hlsPending{pending.key: pending},
	}

	finished := make(chan struct{})
	go func() {
		s.waitForHLSStartOwner(c, "vault", "session", pending)
		close(finished)
	}()
	cancelRequest()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("waiter did not return after request cancellation")
	}
	if pending.stopRequested {
		t.Fatal("pending start was stopped while another same-session owner remained")
	}
	if got := pending.ownerRefs["session"]; got != 1 {
		t.Fatalf("owner references = %d, want 1", got)
	}
}

func TestRemoveHLSDirWaitsForAssetReaders(t *testing.T) {
	dir := t.TempDir()
	stream := &hlsStream{dir: dir}
	stream.assetMu.RLock()
	removed := make(chan struct{})
	go func() {
		removeHLSDir(stream)
		close(removed)
	}()
	select {
	case <-removed:
		t.Fatal("stream directory was removed while an asset reader was active")
	case <-time.After(20 * time.Millisecond):
	}
	stream.assetMu.RUnlock()
	select {
	case <-removed:
	case <-time.After(time.Second):
		t.Fatal("directory cleanup remained blocked after asset reader released")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stream directory still exists after cleanup, stat err=%v", err)
	}
}

func TestHLSContentURLUsesConfiguredPort(t *testing.T) {
	s := &Server{port: "8123"}
	got := s.hlsContentURL("vault", "/dir/video.mp4")
	if !strings.HasPrefix(got, "http://127.0.0.1:8123/") {
		t.Fatalf("content URL = %q, want configured port", got)
	}

	t.Setenv("PORT", "")
	s.port = ""
	got = s.hlsContentURL("vault", "/video.mp4")
	if !strings.HasPrefix(got, "http://127.0.0.1:8080/") {
		t.Fatalf("empty-port content URL = %q, want 8080 fallback", got)
	}
}

func TestHLSCommandCanBeStoppedAndWaited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup test requires Unix signals")
	}
	t.Setenv("CRYP_FFMPEG_BIN", os.Args[0])
	t.Setenv("CRYP_FAKE_FFMPEG_HELPER", "1")
	dir := t.TempDir()
	cmd, cancel, _, err := startHLSCommand(context.Background(), cpuTranscodeProfile(), dir, "http://127.0.0.1/content", "session")
	if err != nil {
		t.Fatalf("startHLSCommand: %v", err)
	}
	stream := &hlsStream{cmd: cmd, cancel: cancel}
	if err := waitForHLSReady(context.Background(), cmd, dir, 2*time.Second); err != nil {
		stopAndWaitHLS(stream)
		t.Fatalf("waitForHLSReady: %v", err)
	}
	stopAndWaitHLS(stream)
	// A killed process reports an exit error; the important contract is that
	// the single Wait owner returns promptly and can be called repeatedly.
	if err := waitHLSCommand(stream); err == nil {
		t.Log("fake ffmpeg exited cleanly")
	}
	if err := waitHLSCommand(stream); err != stream.waitErr {
		t.Fatalf("second wait returned a different error: %v vs %v", err, stream.waitErr)
	}
}

func TestWaitForHLSStopTargetsReportsTimeoutAndCompletion(t *testing.T) {
	done := make(chan struct{})
	s := &Server{}
	targets := hlsStopTargets{streams: []*hlsStream{{doneCh: done}}}
	if s.waitForHLSStopTargets(context.Background(), targets, time.Millisecond) {
		t.Fatal("wait reported completion before stream cleanup")
	}
	close(done)
	if !s.waitForHLSStopTargets(context.Background(), targets, time.Second) {
		t.Fatal("wait did not observe completed stream cleanup")
	}
}

func TestHLSPendingReusableRejectsStoppedAndExpiredStarts(t *testing.T) {
	activeCtx, activeCancel := context.WithCancel(context.Background())
	defer activeCancel()
	active := &hlsPending{ctx: activeCtx}
	if !hlsPendingReusable(active) {
		t.Fatal("active pending start was rejected")
	}

	stopped := &hlsPending{ctx: activeCtx, stopRequested: true}
	if hlsPendingReusable(stopped) {
		t.Fatal("stopped pending start was reused")
	}

	expiredCtx, expiredCancel := context.WithCancel(context.Background())
	expiredCancel()
	expired := &hlsPending{ctx: expiredCtx}
	if hlsPendingReusable(expired) {
		t.Fatal("expired pending start was reused")
	}
}

func TestHLSHardwareProfileFailureCooldown(t *testing.T) {
	t.Setenv("CRYP_FFMPEG_BIN", "ffmpeg-test-hls-cooldown")
	profile := transcodeProfile{
		name:            "vaapi",
		beforeInputArgs: []string{"-vaapi_device", "/dev/dri/renderD128"},
		videoArgs:       []string{"-c:v", "h264_vaapi"},
	}

	defer clearHLSProfileFailure(profile)

	if hlsProfileCoolingDown(profile) {
		t.Fatal("profile was cooling down before a failure was recorded")
	}
	markHLSProfileFailure(profile)
	if !hlsProfileCoolingDown(profile) {
		t.Fatal("failed hardware profile was not cooled down")
	}
	if hlsProfileCoolingDown(cpuTranscodeProfile()) {
		t.Fatal("CPU fallback unexpectedly entered hardware cooldown")
	}
	clearHLSProfileFailure(profile)
	if hlsProfileCoolingDown(profile) {
		t.Fatal("successful profile clear left a stale cooldown")
	}
}

func TestServerShutdownStopsAllHLS(t *testing.T) {
	pendingDone := make(chan struct{})
	close(pendingDone)
	streamDone := make(chan struct{})
	close(streamDone)

	var pendingCancelled, streamCancelled bool
	s := &Server{
		hls: map[string]*hlsStream{
			"stream": {
				vaultID: "vault-b",
				stopCh:  make(chan struct{}),
				doneCh:  streamDone,
				cancel:  func() { streamCancelled = true },
			},
		},
		hlsPending: map[hlsKey]*hlsPending{
			{vaultID: "vault-a", virtualPath: "/video.mp4"}: {
				done:   pendingDone,
				cancel: func() { pendingCancelled = true },
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
	if !pendingCancelled || !streamCancelled {
		t.Fatalf("shutdown cancellation flags = pending %v, stream %v; want both true", pendingCancelled, streamCancelled)
	}
}

func TestServerShutdownHonorsLifecycleBarrierTimeout(t *testing.T) {
	s := &Server{
		hls:        make(map[string]*hlsStream),
		hlsPending: make(map[hlsKey]*hlsPending),
	}
	s.hlsLifeMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want context deadline", err)
	}
	s.hlsLifeMu.Unlock()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after barrier release = %v", err)
	}
}

func TestBeginFileReplacementBlocksNewHLSStarts(t *testing.T) {
	s := &Server{hls: make(map[string]*hlsStream), hlsPending: make(map[hlsKey]*hlsPending)}
	release, err := s.BeginFileReplacement("vault", "/video.mp4")
	if err != nil {
		t.Fatalf("BeginFileReplacement returned error: %v", err)
	}
	acquired := make(chan struct{})
	go func() {
		s.hlsLifeMu.RLock()
		close(acquired)
		s.hlsLifeMu.RUnlock()
	}()
	select {
	case <-acquired:
		t.Fatal("new HLS reader acquired lifecycle lock during replacement")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("HLS reader remained blocked after replacement release")
	}
}
