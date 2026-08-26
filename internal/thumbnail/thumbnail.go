package thumbnail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cryp/internal/crypto"
	"cryp/internal/procgroup"
	"cryp/internal/session"
)

const (
	thumbDir            = "thumbnails"
	thumbWidth          = 320
	thumbHeight         = 180
	imageThumbMax       = 1024
	queueSize           = 1000
	maxRetries          = 1
	failCooldown        = 5 * time.Minute
	maxFailedJobs       = 5000
	ffmpegTimeout       = 2 * time.Minute
	thumbnailJobTimeout = 5 * time.Minute
	probeTimeout        = 10 * time.Second
	detectTimeout       = 5 * time.Second
	maxHEIFBytes        = 512 << 20
	maxProcessLogBytes  = 64 << 10
)

var errThumbnailStale = errors.New("thumbnail job superseded")
var errThumbnailSkipped = errors.New("thumbnail generation skipped")
var errThumbnailQueueFull = errors.New("thumbnail queue is full")
var errThumbnailGenerationFailed = errors.New("thumbnail generation failed")

// videoExtensions lists supported video file extensions
var videoExtensions = map[string]bool{
	".mp4": true, ".webm": true, ".mkv": true, ".avi": true,
	".mov": true, ".m4v": true, ".flv": true, ".wmv": true,
	".mpg": true, ".mpeg": true, ".3gp": true, ".3g2": true,
	".ts": true, ".mts": true, ".m2ts": true, ".vob": true,
	".ogv": true, ".asf": true, ".rm": true, ".rmvb": true,
	".divx": true, ".f4v": true, ".mxf": true, ".h264": true,
	".h265": true, ".hevc": true,
}

var heifExtensions = map[string]bool{
	".heic": true,
	".heif": true,
}

// IsVideo checks if a filename has a video extension
func IsVideo(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return videoExtensions[ext]
}

// IsHEIF checks if a filename has an Apple HEIF/HEIC image extension.
func IsHEIF(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return heifExtensions[ext]
}

// thumbJob represents a thumbnail generation request
type thumbJob struct {
	VaultID    string
	VaultPath  string
	Keys       *crypto.VaultKeys
	FilePath   string // virtual path within vault
	generation uint64
	queueID    uint64
	runID      uint64
	ctx        context.Context
}

// thumbnailRun represents one piece of work that can touch a vault.  Keeping
// cancellation and completion together lets destructive operations wait for a
// concrete owner instead of guessing how long FFmpeg or a directory scan may
// take to stop.
type thumbnailRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// vaultLifecycle is intentionally small and local to the generator.  A vault
// can be quiesced while the global worker continues serving other vaults.
// Queued jobs are counted so a retired vault state can eventually be removed;
// they are discarded by the worker after quiescing and never open the vault.
type vaultLifecycle struct {
	quiescing bool
	retiring  bool
	pending   int
	nextID    uint64
	active    map[uint64]*thumbnailRun
}

// Generator manages async thumbnail generation with a single-worker queue.
// It generates thumbnails by having FFmpeg fetch the video via the local
// HTTP server's Range-enabled content endpoint. This means:
//   - No plaintext temp files — decryption happens on-the-fly via HTTP Range
//   - MP4 moov-at-end works — FFmpeg sends Range requests to seek
//   - Only a few MB of data is transferred for a single frame extraction
type Generator struct {
	vaultDir       string
	sessions       *session.Store
	port           string
	jobs           chan thumbJob
	ffmpeg         ffmpegConfig
	wg             sync.WaitGroup
	mu             sync.Mutex
	commitMu       sync.Mutex
	queued         map[string]struct{}
	failed         map[string]time.Time
	generations    map[string]uint64
	generationRefs map[string]int
	vaults         map[string]*vaultLifecycle
	resumePending  map[string]struct{}
	stopped        bool
	ctx            context.Context
	cancel         context.CancelFunc
	stopOnce       sync.Once
}

type ffmpegConfig struct {
	bin      string
	hwaccel  string
	attempts []ffmpegAttempt
}

type ffmpegAttempt struct {
	hwaccel             string
	hwaccelOutputFormat string
}

// NewGenerator creates a new thumbnail generator.
// sessions and port are used to create temporary auth sessions for internal
// FFmpeg HTTP requests against the local server's content endpoint.
func NewGenerator(vaultDir string, sessions *session.Store, port string) *Generator {
	cfg := ffmpegConfig{
		bin:     thumbnailFFmpegBinary(),
		hwaccel: strings.TrimSpace(os.Getenv("CRYP_FFMPEG_HWACCEL")),
	}
	if cfg.hwaccel == "" {
		cfg.hwaccel = "auto"
	}

	candidates := buildFFmpegAttempts(cfg)
	cfg.attempts = probeFFmpegAttempts(cfg.bin, candidates)
	if len(cfg.attempts) == 1 && cfg.attempts[0].hwaccel == "" {
		log.Printf("thumbnail: active backend order: cpu")
	} else {
		log.Printf("thumbnail: active backend order: %s", formatFFmpegAttempts(cfg.attempts))
	}

	ctx, cancel := context.WithCancel(context.Background())
	g := &Generator{
		vaultDir:       vaultDir,
		sessions:       sessions,
		port:           port,
		jobs:           make(chan thumbJob, queueSize),
		ffmpeg:         cfg,
		queued:         make(map[string]struct{}),
		failed:         make(map[string]time.Time),
		generations:    make(map[string]uint64),
		generationRefs: make(map[string]int),
		vaults:         make(map[string]*vaultLifecycle),
		resumePending:  make(map[string]struct{}),
		ctx:            ctx,
		cancel:         cancel,
	}
	g.wg.Add(1)
	go g.worker()
	return g
}

func thumbnailFFmpegBinary() string {
	if bin := strings.TrimSpace(os.Getenv("CRYP_FFMPEG_BIN")); bin != "" {
		return bin
	}
	return "ffmpeg"
}

func buildFFmpegAttempts(cfg ffmpegConfig) []ffmpegAttempt {
	hw := strings.ToLower(strings.TrimSpace(cfg.hwaccel))
	if hw == "none" || hw == "off" || hw == "cpu" {
		return []ffmpegAttempt{{}}
	}

	if hw != "" && hw != "auto" {
		return []ffmpegAttempt{
			{
				hwaccel:             hw,
				hwaccelOutputFormat: defaultOutputFormat(hw),
			},
			{},
		}
	}

	available := detectFFmpegHwaccels(cfg.bin)
	order := []string{"vaapi", "qsv", "cuda", "d3d11va", "videotoolbox"}
	attempts := make([]ffmpegAttempt, 0, len(order)+1)
	for _, name := range order {
		if !available[name] {
			continue
		}
		attempts = append(attempts, ffmpegAttempt{
			hwaccel:             name,
			hwaccelOutputFormat: defaultOutputFormat(name),
		})
	}

	attempts = append(attempts, ffmpegAttempt{})
	return attempts
}

func detectFFmpegHwaccels(bin string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-hide_banner", "-hwaccels")
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd)
	out := procgroup.NewTailBuffer(maxProcessLogBytes)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return map[string]bool{}
	}

	available := make(map[string]bool)
	lines := strings.Split(out.String(), "\n")
	for _, line := range lines {
		item := strings.ToLower(strings.TrimSpace(line))
		if item == "" || strings.Contains(item, ":") {
			continue
		}
		available[item] = true
	}
	return available
}

func defaultOutputFormat(hwaccel string) string {
	switch strings.ToLower(strings.TrimSpace(hwaccel)) {
	case "vaapi":
		return "vaapi"
	case "cuda":
		return "cuda"
	default:
		return ""
	}
}

func probeFFmpegAttempts(bin string, attempts []ffmpegAttempt) []ffmpegAttempt {
	if len(attempts) == 0 {
		return []ffmpegAttempt{{}}
	}

	valid := make([]ffmpegAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.hwaccel == "" {
			continue
		}
		if canUseFFmpegAttempt(bin, attempt) {
			valid = append(valid, attempt)
		}
	}

	valid = append(valid, ffmpegAttempt{})
	return valid
}

func canUseFFmpegAttempt(bin string, attempt ffmpegAttempt) bool {
	return canUseFFmpegAttemptWithTimeout(bin, attempt, probeTimeout)
}

func canUseFFmpegAttemptWithTimeout(bin string, attempt ffmpegAttempt, timeout time.Duration) bool {
	return canUseFFmpegAttemptWithRunner(bin, attempt, timeout, runFFmpegProbeCommand)
}

type ffmpegProbeRunner func(context.Context, string, []string) error

func canUseFFmpegAttemptWithRunner(bin string, attempt ffmpegAttempt, timeout time.Duration, run ffmpegProbeRunner) bool {
	if run == nil || timeout <= 0 {
		return false
	}
	probeFile, err := os.CreateTemp("", "cryp-ffmpeg-probe-*.mp4")
	if err != nil {
		return false
	}
	probePath := probeFile.Name()
	if err := closeTempFile(probeFile, probePath); err != nil {
		return false
	}
	defer func() { _ = os.Remove(probePath) }()

	// Keep the encoder and decoder probes on independent deadlines.  The
	// encoder probe can legitimately consume most of probeTimeout on a cold
	// driver; sharing its context with the decoder would make the second probe
	// inherit the already-expired budget and incorrectly disable a usable
	// hardware backend.
	createCtx, createCancel := context.WithTimeout(context.Background(), timeout)
	createArgs := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "lavfi",
		"-i", "testsrc2=size=128x72:rate=30",
		"-t", "1",
		"-c:v", "h264",
		"-y", probePath,
	}
	if err := run(createCtx, bin, createArgs); err != nil {
		createCancel()
		return false
	}
	createCancel()

	probeArgs := make([]string, 0, 16)
	probeArgs = append(probeArgs, "-hide_banner", "-loglevel", "error")
	probeArgs = appendAttemptHwArgs(probeArgs, attempt)
	probeArgs = append(probeArgs, "-i", probePath, "-frames:v", "1", "-f", "null", "-")

	decodeCtx, decodeCancel := context.WithTimeout(context.Background(), timeout)
	defer decodeCancel()
	return run(decodeCtx, bin, probeArgs) == nil
}

func runFFmpegProbeCommand(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd)
	out := procgroup.NewTailBuffer(maxProcessLogBytes)
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	return err
}

func appendAttemptHwArgs(args []string, attempt ffmpegAttempt) []string {
	if attempt.hwaccel == "" {
		return args
	}
	args = append(args, "-hwaccel", attempt.hwaccel)
	if attempt.hwaccel == "vaapi" {
		if device := findVAAPIDevice(); device != "" {
			args = append(args, "-hwaccel_device", device)
		}
	}
	if attempt.hwaccelOutputFormat != "" {
		args = append(args, "-hwaccel_output_format", attempt.hwaccelOutputFormat)
	}
	return args
}

func findVAAPIDevice() string {
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

func formatFFmpegAttempts(attempts []ffmpegAttempt) string {
	if len(attempts) == 0 {
		return "cpu"
	}
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		if a.hwaccel == "" {
			parts = append(parts, "cpu")
			continue
		}
		parts = append(parts, a.hwaccel)
	}
	return strings.Join(parts, " -> ")
}

func (g *Generator) baseContext() context.Context {
	if g != nil && g.ctx != nil {
		return g.ctx
	}
	return context.Background()
}

// lifecycleLocked returns the state for a vault. Callers must hold g.mu.
func (g *Generator) lifecycleLocked(vaultID string) *vaultLifecycle {
	if g.vaults == nil {
		g.vaults = make(map[string]*vaultLifecycle)
	}
	state := g.vaults[vaultID]
	if state == nil {
		state = &vaultLifecycle{active: make(map[uint64]*thumbnailRun)}
		g.vaults[vaultID] = state
	}
	if state.active == nil {
		state.active = make(map[uint64]*thumbnailRun)
	}
	return state
}

// maybeRetireLocked removes a tombstone after all queued and active work has
// gone away. A tombstone remains quiescing while queued jobs are still waiting
// in the global channel, so a late worker cannot recreate a deleted vault.
// Callers must hold g.mu.
func (g *Generator) maybeRetireLocked(vaultID string) {
	state := g.vaults[vaultID]
	if state == nil || !state.retiring {
		return
	}
	if state.pending == 0 && len(state.active) == 0 {
		g.purgeVaultCachesLocked(vaultID)
		delete(g.resumePending, vaultID)
		delete(g.vaults, vaultID)
	}
}

// purgeVaultCachesLocked releases per-file bookkeeping after a vault has no
// queued or active owners left. Without this cleanup, generations and failure
// cooldowns would retain every deleted vault's virtual paths for the lifetime
// of the process even though the lifecycle tombstone was gone.
func (g *Generator) purgeVaultCachesLocked(vaultID string) {
	prefix := vaultID + "\x00"
	for key := range g.queued {
		if strings.HasPrefix(key, prefix) {
			delete(g.queued, key)
		}
	}
	for key := range g.failed {
		if strings.HasPrefix(key, prefix) {
			delete(g.failed, key)
		}
	}
	for key := range g.generations {
		if strings.HasPrefix(key, prefix) {
			delete(g.generations, key)
		}
	}
	for key := range g.generationRefs {
		if strings.HasPrefix(key, prefix) {
			delete(g.generationRefs, key)
		}
	}
}

// retainGenerationLocked records one queued or active job that still relies on
// the current per-path generation token. Callers must hold g.mu.
func (g *Generator) retainGenerationLocked(key string) {
	if g.generationRefs == nil {
		g.generationRefs = make(map[string]int)
	}
	g.generationRefs[key]++
}

// releaseGenerationLocked drops a job's generation ownership. The token is no
// longer needed once no queued/active job can commit an older thumbnail.
// Callers must hold g.mu.
func (g *Generator) releaseGenerationLocked(key string) {
	refs := g.generationRefs[key]
	if refs > 1 {
		g.generationRefs[key] = refs - 1
		return
	}
	delete(g.generationRefs, key)
	g.maybePurgeGenerationLocked(key)
}

// maybePurgeGenerationLocked removes an idle token without disturbing a newer
// queued generation for the same virtual path. Callers must hold g.mu.
func (g *Generator) maybePurgeGenerationLocked(key string) {
	if g.generationRefs[key] != 0 {
		return
	}
	if _, queued := g.queued[key]; queued {
		return
	}
	delete(g.generations, key)
}

// beginJob claims a queued job for execution. The claim is the last gate
// before opening a vault file, so a quiesced/retired vault is safe even when a
// job was already buffered in the worker channel.
func (g *Generator) beginJob(job *thumbJob) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.lifecycleLocked(job.VaultID)
	if job.queueID != 0 && state.pending > 0 {
		state.pending--
	}
	if g.stopped || state.quiescing || state.retiring {
		g.maybeRetireLocked(job.VaultID)
		return false
	}
	state.nextID++
	runID := state.nextID
	ctx, cancel := context.WithCancel(g.baseContext())
	state.active[runID] = &thumbnailRun{cancel: cancel, done: make(chan struct{})}
	job.runID = runID
	job.ctx = ctx
	return true
}

func (g *Generator) endJob(job thumbJob) {
	if job.runID == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.vaults[job.VaultID]
	if state == nil {
		return
	}
	if run, ok := state.active[job.runID]; ok {
		delete(state.active, job.runID)
		run.cancel()
		close(run.done)
	}
	if len(state.active) == 0 && state.pending == 0 {
		if _, requested := g.resumePending[job.VaultID]; requested && !state.retiring {
			state.quiescing = false
			delete(g.resumePending, job.VaultID)
		}
	}
	g.maybeRetireLocked(job.VaultID)
}

// QuiesceVault prevents new thumbnail work for a vault, cancels active
// FFmpeg work, and waits for the concrete owners to finish. Queued jobs
// remain in the shared channel but are discarded by beginJob, so they never
// touch a path after this method returns successfully.
func (g *Generator) QuiesceVault(ctx context.Context, vaultID string) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return nil
	}
	state := g.lifecycleLocked(vaultID)
	state.quiescing = true
	runs := make([]*thumbnailRun, 0, len(state.active))
	for _, run := range state.active {
		runs = append(runs, run)
	}
	g.mu.Unlock()

	for _, run := range runs {
		run.cancel()
	}
	for _, run := range runs {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ResumeVault reopens thumbnail work after a destructive operation was
// deferred. If cancellation is still winding down, the tombstone stays in
// place; a later quiesce retry will wait for the same owners instead of
// allowing a new FFmpeg process to race them.
func (g *Generator) ResumeVault(vaultID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resumePending == nil {
		g.resumePending = make(map[string]struct{})
	}
	state := g.vaults[vaultID]
	if state == nil || state.retiring {
		return
	}
	if len(state.active) == 0 && state.pending == 0 {
		state.quiescing = false
		delete(g.resumePending, vaultID)
	} else {
		g.resumePending[vaultID] = struct{}{}
	}
}

// ForgetVault leaves a short-lived tombstone when queued jobs are still in the
// channel, then removes it automatically as the worker discards those jobs.
// This avoids retaining one lifecycle object forever without reopening a
// deleted vault to late work.
func (g *Generator) ForgetVault(vaultID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.vaults[vaultID]
	if state == nil {
		return
	}
	state.retiring = true
	state.quiescing = true
	g.maybeRetireLocked(vaultID)
}

// Stop gracefully shuts down the generator
func (g *Generator) Stop() {
	g.stopOnce.Do(func() {
		g.mu.Lock()
		g.stopped = true
		g.mu.Unlock()
		if g.cancel != nil {
			g.cancel()
		}
		if g.jobs != nil {
			g.mu.Lock()
			close(g.jobs)
			g.mu.Unlock()
		}
		g.wg.Wait()
	})
}

// Enqueue adds a thumbnail generation job to the queue.
// Non-blocking: drops the job if queue is full.
func (g *Generator) Enqueue(vaultID, vaultPath string, keys *crypto.VaultKeys, virtualPath string) {
	if err := g.tryEnqueue(vaultID, vaultPath, keys, virtualPath); errors.Is(err, errThumbnailQueueFull) {
		log.Printf("thumbnail: queue full, dropping %s", virtualPath)
	}
}

// EnqueueContext guarantees admission unless the caller is cancelled or the
// generator is unavailable. Rebuild tasks use it to apply natural backpressure
// instead of silently dropping derived assets when a large vault fills the
// bounded worker queue.
func (g *Generator) EnqueueContext(ctx context.Context, vaultID, vaultPath string, keys *crypto.VaultKeys, virtualPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		err := g.tryEnqueue(vaultID, vaultPath, keys, virtualPath)
		if !errors.Is(err, errThumbnailQueueFull) {
			return err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// WaitVaultIdle waits until every admitted thumbnail for a vault has either
// completed or failed. Index rebuild tasks use this completion barrier so a
// successful rebuild means its screenshot work is no longer merely queued.
func (g *Generator) WaitVaultIdle(ctx context.Context, vaultID string) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		g.mu.Lock()
		state := g.vaults[vaultID]
		idle := state == nil || (state.pending == 0 && len(state.active) == 0)
		failed := false
		prefix := vaultID + "\x00"
		if idle {
			for key := range g.failed {
				if strings.HasPrefix(key, prefix) {
					failed = true
					break
				}
			}
		}
		g.mu.Unlock()
		if idle {
			if failed {
				return fmt.Errorf("%w for vault %s", errThumbnailGenerationFailed, vaultID)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// PrepareVaultRebuild drains existing work and removes the complete derived
// thumbnail cache for a vault. Rebuild will repopulate it from the encrypted
// source of truth, which also removes orphaned screenshots for deleted files.
// Admission is reopened before returning so the caller can enqueue the fresh
// generation; the cache removal itself is protected by the quiesce barrier.
func (g *Generator) PrepareVaultRebuild(ctx context.Context, vaultID string) error {
	if g == nil || strings.TrimSpace(g.vaultDir) == "" || strings.TrimSpace(vaultID) == "" {
		return errors.New("thumbnail generator is unavailable")
	}
	// Establish the admission barrier before waiting. Otherwise a producer can
	// enqueue work between WaitVaultIdle returning and cache removal.
	if err := g.QuiesceVault(ctx, vaultID); err != nil {
		return err
	}
	g.commitMu.Lock()
	defer g.commitMu.Unlock()
	if err := os.RemoveAll(filepath.Join(g.vaultDir, vaultID, thumbDir)); err != nil {
		return err
	}
	g.mu.Lock()
	g.purgeVaultCachesLocked(vaultID)
	g.mu.Unlock()
	g.ResumeVault(vaultID)
	return nil
}

func (g *Generator) tryEnqueue(vaultID, vaultPath string, keys *crypto.VaultKeys, virtualPath string) error {
	if g == nil || keys == nil || strings.TrimSpace(g.vaultDir) == "" || strings.TrimSpace(vaultID) == "" {
		return errors.New("thumbnail generator is unavailable")
	}
	if g.HasThumbnail(vaultID, virtualPath) {
		return nil
	}
	// Jobs outlive the request/session that enqueued them. Keep an owned copy
	// so session expiry or logout cannot zero the key slices while the worker is
	// still decrypting a queued file.
	keysCopy := keys.Clone()
	transferred := false
	defer func() {
		if !transferred {
			zeroVaultKeys(keysCopy)
		}
	}()

	key := thumbJobKey(vaultID, virtualPath)
	now := time.Now()
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return errors.New("thumbnail generator is stopped")
	}
	state := g.lifecycleLocked(vaultID)
	if state.quiescing || state.retiring {
		g.mu.Unlock()
		return errors.New("thumbnail vault is unavailable")
	}
	g.pruneFailedLocked(now)
	if _, ok := g.queued[key]; ok {
		g.mu.Unlock()
		return nil
	}
	if failedUntil, ok := g.failed[key]; ok {
		if now.Before(failedUntil) {
			g.mu.Unlock()
			return nil
		}
		delete(g.failed, key)
	}
	g.queued[key] = struct{}{}
	state.nextID++
	queueID := state.nextID
	state.pending++
	job := thumbJob{
		VaultID:    vaultID,
		VaultPath:  vaultPath,
		Keys:       keysCopy,
		FilePath:   virtualPath,
		generation: g.generations[key],
		queueID:    queueID,
	}
	// Keep the non-blocking send under the same lock used by Stop. This closes
	// the send/close race that otherwise could panic during server shutdown.
	select {
	case g.jobs <- job:
		g.retainGenerationLocked(key)
		transferred = true
	default:
		delete(g.queued, key)
		state.pending--
		g.maybeRetireLocked(vaultID)
		g.mu.Unlock()
		return errThumbnailQueueFull
	}
	g.mu.Unlock()
	return nil
}

func (g *Generator) pruneFailedLocked(now time.Time) {
	for key, until := range g.failed {
		if !now.Before(until) {
			delete(g.failed, key)
		}
	}
	for len(g.failed) > maxFailedJobs {
		for key := range g.failed {
			delete(g.failed, key)
			break
		}
	}
}

// GetPath returns the thumbnail file path if it exists, or empty string
func (g *Generator) GetPath(vaultID, virtualPath string) string {
	if g == nil || strings.TrimSpace(g.vaultDir) == "" || strings.TrimSpace(vaultID) == "" || virtualPath == "" {
		return ""
	}
	p := g.thumbPath(vaultID, virtualPath)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// HasThumbnail checks if a cached thumbnail exists
func (g *Generator) HasThumbnail(vaultID, virtualPath string) bool {
	return g.GetPath(vaultID, virtualPath) != ""
}

// DeleteThumbnail removes a cached thumbnail for a file if it exists.
func (g *Generator) DeleteThumbnail(vaultID, virtualPath string) {
	if g == nil || strings.TrimSpace(g.vaultDir) == "" || strings.TrimSpace(vaultID) == "" || virtualPath == "" {
		return
	}
	p := g.thumbPath(vaultID, virtualPath)
	key := thumbJobKey(vaultID, virtualPath)
	g.commitMu.Lock()
	defer g.commitMu.Unlock()
	g.mu.Lock()
	// Invalidate any in-flight job as well as the cached file. The generation
	// check prevents an older FFmpeg result from being committed after a file
	// at the same virtual path is replaced.
	if g.generations == nil {
		g.generations = make(map[string]uint64)
	}
	g.generations[key]++
	delete(g.queued, key)
	delete(g.failed, key)
	g.maybePurgeGenerationLocked(key)
	os.Remove(p) // ignore error if not exists
	g.mu.Unlock()
}

// thumbPath returns the on-disk path for a thumbnail
func (g *Generator) thumbPath(vaultID, virtualPath string) string {
	cacheKey := virtualPath
	if IsHEIF(virtualPath) {
		cacheKey += "\x00heif-v2"
	}
	hash := sha256.Sum256([]byte(cacheKey))
	name := fmt.Sprintf("%x.c9r", hash)
	return filepath.Join(g.vaultDir, vaultID, thumbDir, name)
}

func thumbJobKey(vaultID, virtualPath string) string {
	return vaultID + "\x00" + virtualPath
}

// worker processes thumbnail generation jobs sequentially
func (g *Generator) worker() {
	defer g.wg.Done()
	for job := range g.jobs {
		if !g.beginJob(&job) {
			// A queued job may outlive a vault deletion or generator shutdown.
			// Its key copy is still owned by the worker even though no I/O is
			// performed, so release it here rather than waiting for GC.
			g.discardJob(job)
			continue
		}
		var generateErr error
		// Skip if already exists
		if g.HasThumbnail(job.VaultID, job.FilePath) {
			generateErr = nil
		} else {
			generateErr = g.generate(job)
		}
		g.finishJob(job, generateErr)
		// Keep the lifecycle owner until generation bookkeeping is complete.
		// Otherwise a retiring vault could purge its maps in endJob and have
		// finishJob recreate a failure entry immediately afterwards.
		g.endJob(job)
		if generateErr != nil && !errors.Is(generateErr, errThumbnailStale) && !errors.Is(generateErr, errThumbnailSkipped) {
			log.Printf("thumbnail: failed to generate for %s: %v", job.FilePath, generateErr)
		}
	}
}

func (g *Generator) discardJob(job thumbJob) {
	defer zeroVaultKeys(job.Keys)
	key := thumbJobKey(job.VaultID, job.FilePath)
	g.mu.Lock()
	if g.generations[key] == job.generation {
		delete(g.queued, key)
	}
	g.releaseGenerationLocked(key)
	g.maybeRetireLocked(job.VaultID)
	g.mu.Unlock()
}

func (g *Generator) finishJob(job thumbJob, err error) {
	defer zeroVaultKeys(job.Keys)
	key := thumbJobKey(job.VaultID, job.FilePath)
	g.mu.Lock()
	defer g.mu.Unlock()
	defer g.releaseGenerationLocked(key)
	// A replacement may have queued a newer generation for the same path while
	// this job was running. Do not let the old job clear the newer job's queue
	// marker or failure cooldown.
	currentGeneration := g.generations[key]
	isCurrent := currentGeneration == job.generation
	if isCurrent {
		delete(g.queued, key)
	}
	if errors.Is(err, errThumbnailStale) {
		if isCurrent {
			delete(g.failed, key)
		}
		return
	}
	if errors.Is(err, errThumbnailSkipped) {
		if isCurrent {
			g.failed[key] = time.Now().Add(failCooldown)
		}
		return
	}
	if !isCurrent {
		return
	}
	if err != nil {
		g.failed[key] = time.Now().Add(failCooldown)
		return
	}
	delete(g.failed, key)
}

// generate creates a thumbnail by having FFmpeg fetch the video via the local
// HTTP server. The server supports Range requests, so FFmpeg can seek to read
// the moov atom (even at end of file) and extract a frame — all without any
// plaintext ever touching disk.
func (g *Generator) generate(job thumbJob) error {
	baseCtx := job.ctx
	if baseCtx == nil {
		baseCtx = g.baseContext()
	}
	if err := baseCtx.Err(); err != nil {
		return err
	}
	if !g.thumbnailJobCurrent(job) {
		return errThumbnailStale
	}
	jobCtx, jobCancel := context.WithTimeout(baseCtx, thumbnailJobTimeout)
	defer jobCancel()
	baseCtx = jobCtx
	vault := &crypto.Vault{
		ID:   job.VaultID,
		Path: job.VaultPath,
		Keys: job.Keys,
	}
	if encPath, err := vault.ResolveExistingFilePath(job.FilePath); err == nil {
		if info, statErr := os.Stat(encPath); statErr == nil && crypto.CipherSize2PlaintextSize(info.Size()) == 0 {
			return errThumbnailSkipped
		}
	}

	if IsHEIF(job.FilePath) {
		return g.generateHEIFWithContext(job, baseCtx)
	}
	if g.sessions == nil {
		return errors.New("session store unavailable")
	}

	keysCopy := job.Keys.Clone()
	if keysCopy == nil {
		return errors.New("missing thumbnail keys")
	}

	// Create a short-lived internal session so FFmpeg can authenticate
	sessionID, err := g.sessions.Create(job.VaultID, job.VaultPath, keysCopy)
	zeroVaultKeys(keysCopy)
	if err != nil {
		return fmt.Errorf("create temp session: %w", err)
	}
	defer g.sessions.Delete(sessionID)

	// Build the internal URL for the content endpoint
	contentURL := fmt.Sprintf("http://127.0.0.1:%s/api/vaults/%s/files/content?probe=1&path=%s",
		g.port, job.VaultID, url.QueryEscape(job.FilePath))

	// Ensure thumbnail directory exists
	outPath := g.thumbPath(job.VaultID, job.FilePath)
	if err := os.MkdirAll(filepath.Dir(outPath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(outPath), ".thumb-*.jpg")
	if err != nil {
		return fmt.Errorf("create thumbnail temp: %w", err)
	}
	thumbTmp := tmpFile.Name()
	if err := closeTempFile(tmpFile, thumbTmp); err != nil {
		return fmt.Errorf("close thumbnail temp: %w", err)
	}
	defer func() { _ = os.Remove(thumbTmp) }()

	var lastErr error
	for _, attempt := range g.ffmpeg.attempts {
		if err := baseCtx.Err(); err != nil {
			return err
		}
		ffmpegArgs := make([]string, 0, 24)
		ffmpegArgs = appendAttemptHwArgs(ffmpegArgs, attempt)

		videoFilter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black",
			thumbWidth, thumbHeight, thumbWidth, thumbHeight)
		if attempt.hwaccel != "" && attempt.hwaccelOutputFormat != "" {
			// Hardware decoders return device surfaces; thumbnails are software
			// JPEGs, so explicitly download frames before scale/pad.
			videoFilter = "hwdownload,format=nv12," + videoFilter
		}

		ffmpegArgs = append(ffmpegArgs,
			"-probesize", "100M",
			"-analyzeduration", "100M",
			// Pass session cookie via HTTP headers for authentication
			"-headers", fmt.Sprintf("Cookie: session_id=%s\r\n", sessionID),
			// Input from local HTTP server (supports Range for seeking)
			"-i", contentURL,
			// Extract a single frame
			"-frames:v", "1",
			// Scale to thumbnail size with padding to maintain aspect ratio
			"-vf", videoFilter,
			"-q:v", "5",
			"-f", "image2",
			"-y", thumbTmp,
		)

		stderrBuf := procgroup.NewTailBuffer(maxProcessLogBytes)
		cmdCtx, cancel := context.WithTimeout(baseCtx, ffmpegTimeout)
		cmd := exec.CommandContext(cmdCtx, g.ffmpeg.bin, ffmpegArgs...)
		cmd.WaitDelay = 2 * time.Second
		procgroup.Configure(cmd)
		cmd.Stderr = stderrBuf

		err := cmd.Run()
		ctxErr := cmdCtx.Err()
		cancel()
		if err != nil {
			if ctxErr != nil {
				return ctxErr
			}
			errOut := stderrBuf.String()
			if len(errOut) > 500 {
				errOut = errOut[len(errOut)-500:]
			}

			if attempt.hwaccel != "" {
				log.Printf("thumbnail: hwaccel=%s failed, fallback next backend: %v", attempt.hwaccel, err)
			}
			if ctxErr == context.DeadlineExceeded {
				lastErr = fmt.Errorf("ffmpeg timeout after %s: %s", ffmpegTimeout, errOut)
			} else {
				lastErr = fmt.Errorf("ffmpeg: %w: %s", err, errOut)
			}
			continue
		}
		if info, statErr := os.Stat(thumbTmp); statErr != nil || info.Size() == 0 {
			lastErr = fmt.Errorf("ffmpeg produced no thumbnail")
			continue
		}

		lastErr = nil
		break
	}

	if lastErr != nil {
		return lastErr
	}

	// Encrypt the thumbnail JPEG before storing
	if err := g.encryptThumbnail(job, thumbTmp, outPath); err != nil {
		return fmt.Errorf("encrypt thumbnail: %w", err)
	}

	return nil
}

func (g *Generator) generateHEIF(job thumbJob) error {
	baseCtx := job.ctx
	if baseCtx == nil {
		baseCtx = g.baseContext()
	}
	return g.generateHEIFWithContext(job, baseCtx)
}

func (g *Generator) generateHEIFWithContext(job thumbJob, baseCtx context.Context) error {
	outPath := g.thumbPath(job.VaultID, job.FilePath)
	if err := os.MkdirAll(filepath.Dir(outPath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	tmpDir, err := os.MkdirTemp(filepath.Dir(outPath), ".heif-thumb-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	heifPath := filepath.Join(tmpDir, "input"+strings.ToLower(filepath.Ext(job.FilePath)))
	fullJPEGPath := filepath.Join(tmpDir, "full.jpg")
	tmpFile, err := os.CreateTemp(filepath.Dir(outPath), ".thumb-*.jpg")
	if err != nil {
		return fmt.Errorf("create thumbnail temp: %w", err)
	}
	thumbTmp := tmpFile.Name()
	if err := closeTempFile(tmpFile, thumbTmp); err != nil {
		return fmt.Errorf("close thumbnail temp: %w", err)
	}
	defer func() { _ = os.Remove(thumbTmp) }()

	if err := g.writeDecryptedFileWithContext(job, heifPath, baseCtx); err != nil {
		return fmt.Errorf("decrypt heif: %w", err)
	}
	if err := baseCtx.Err(); err != nil {
		return err
	}

	heifErr := procgroup.NewTailBuffer(maxProcessLogBytes)
	convertCtx, convertCancel := context.WithTimeout(baseCtx, ffmpegTimeout)
	convertCmd := exec.CommandContext(convertCtx, "heif-convert", heifPath, fullJPEGPath)
	convertCmd.WaitDelay = 2 * time.Second
	procgroup.Configure(convertCmd)
	convertCmd.Stderr = heifErr
	convertErr := convertCmd.Run()
	convertCtxErr := convertCtx.Err()
	convertCancel()
	if convertErr != nil {
		if convertCtxErr != nil {
			return convertCtxErr
		}
		errOut := heifErr.String()
		if len(errOut) > 500 {
			errOut = errOut[len(errOut)-500:]
		}
		return fmt.Errorf("heif-convert: %w: %s", convertErr, errOut)
	}
	if info, statErr := os.Stat(fullJPEGPath); statErr != nil {
		return fmt.Errorf("heif-convert produced no image: %w", statErr)
	} else if info.Size() > maxHEIFBytes {
		return fmt.Errorf("converted heif exceeds %d-byte limit", maxHEIFBytes)
	}

	ffmpegArgs := []string{
		"-i", fullJPEGPath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease",
			imageThumbMax, imageThumbMax),
		"-q:v", "3",
		"-f", "image2",
		"-y", thumbTmp,
	}

	ffmpegErr := procgroup.NewTailBuffer(maxProcessLogBytes)
	ffmpegCtx, ffmpegCancel := context.WithTimeout(baseCtx, ffmpegTimeout)
	cmd := exec.CommandContext(ffmpegCtx, g.ffmpeg.bin, ffmpegArgs...)
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd)
	cmd.Stderr = ffmpegErr
	ffmpegRunErr := cmd.Run()
	ffmpegCtxErr := ffmpegCtx.Err()
	ffmpegCancel()
	if ffmpegRunErr != nil {
		if ffmpegCtxErr != nil {
			return ffmpegCtxErr
		}
		errOut := ffmpegErr.String()
		if len(errOut) > 500 {
			errOut = errOut[len(errOut)-500:]
		}
		return fmt.Errorf("ffmpeg scale heif: %w: %s", ffmpegRunErr, errOut)
	}

	if err := g.encryptThumbnail(job, thumbTmp, outPath); err != nil {
		return fmt.Errorf("encrypt thumbnail: %w", err)
	}
	return nil
}

func (g *Generator) thumbnailJobCurrent(job thumbJob) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generations[thumbJobKey(job.VaultID, job.FilePath)] == job.generation
}

func (g *Generator) encryptThumbnail(job thumbJob, srcPath, outPath string) error {
	// Serialize the final commit with DeleteThumbnail. This closes the
	// check/rename window where an upload could invalidate a job just after it
	// checked its generation, without holding the state mutex during disk I/O.
	g.commitMu.Lock()
	defer g.commitMu.Unlock()
	g.mu.Lock()
	current := g.generations[thumbJobKey(job.VaultID, job.FilePath)] == job.generation
	g.mu.Unlock()
	if !current {
		return errThumbnailStale
	}
	return g.encryptFile(srcPath, outPath, job.Keys.MasterKey)
}

func (g *Generator) writeDecryptedFile(job thumbJob, outPath string) error {
	ctx := g.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return g.writeDecryptedFileWithContext(job, outPath, ctx)
}

func (g *Generator) writeDecryptedFileWithContext(job thumbJob, outPath string, ctx context.Context) error {
	vault := &crypto.Vault{
		ID:   job.VaultID,
		Path: job.VaultPath,
		Keys: job.Keys,
	}

	encPath, err := vault.ResolveExistingFilePath(job.FilePath)
	if err != nil {
		return err
	}
	in, err := os.Open(encPath)
	if err != nil {
		return err
	}
	defer in.Close()
	if info, statErr := in.Stat(); statErr == nil {
		if plaintextSize := crypto.CipherSize2PlaintextSize(info.Size()); plaintextSize > maxHEIFBytes {
			return fmt.Errorf("heif input exceeds %d-byte limit", maxHEIFBytes)
		}
	}

	header, err := crypto.ReadFileHeader(in, job.Keys.MasterKey)
	if err != nil {
		return err
	}
	reader, err := crypto.NewDecryptingReader(in, header.ContentKey, header.Nonce)
	if err != nil {
		return err
	}
	defer reader.Release()

	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()

	bufp := crypto.CopyBufPool.Get().(*[]byte)
	defer crypto.CopyBufPool.Put(bufp)
	var written int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, readErr := reader.Read(*bufp)
		if n > 0 {
			written += int64(n)
			if written > maxHEIFBytes {
				return fmt.Errorf("heif input exceeds %d-byte limit", maxHEIFBytes)
			}
			if _, writeErr := out.Write((*bufp)[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}

	crypto.DropFileCache(in)
	crypto.DropFileCache(out)
	return nil
}

func zeroVaultKeys(keys *crypto.VaultKeys) {
	if keys != nil {
		keys.Zero()
	}
}

// closeTempFile makes temporary-file ownership explicit. A close failure can
// leave buffered data unwritten (or a descriptor unusable for the next
// process), so callers fail before handing the path to FFmpeg and remove the
// partial file best-effort.
func closeTempFile(file *os.File, path string) error {
	if file == nil {
		return nil
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// encryptFile reads a plaintext file, encrypts it using the same format as vault
// content files (header + AES-256-GCM chunks), and writes the result to outPath.
func (g *Generator) encryptFile(srcPath, outPath string, masterKey []byte) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	tmpFile, err := os.CreateTemp(filepath.Dir(outPath), ".thumb-encrypt-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	out := tmpFile
	defer out.Close()

	contentKey := make([]byte, crypto.MasterKeySize)
	if _, err := rand.Read(contentKey); err != nil {
		return err
	}

	header, err := crypto.WriteFileHeader(out, masterKey, contentKey)
	if err != nil {
		return err
	}

	writer, err := crypto.NewEncryptingWriter(out, header.ContentKey, header.Nonce)
	if err != nil {
		return err
	}

	if _, err := crypto.CopyWithCacheDrop(writer, src, out); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	if err := out.Sync(); err != nil {
		return err
	}
	crypto.DropFileCache(src)
	crypto.DropFileCache(out)
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return err
	}

	return nil
}

// DecryptThumbnail opens an encrypted thumbnail and returns a reader for the
// decrypted JPEG content. Caller must call release() when done.
func DecryptThumbnail(path string, masterKey []byte) (io.Reader, func(), error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}

	header, err := crypto.ReadFileHeader(file, masterKey)
	if err != nil {
		file.Close()
		return nil, nil, err
	}

	reader, err := crypto.NewDecryptingReader(file, header.ContentKey, header.Nonce)
	if err != nil {
		file.Close()
		return nil, nil, err
	}

	release := func() {
		reader.Release()
		file.Close()
	}
	return reader, release, nil
}
