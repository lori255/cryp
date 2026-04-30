package thumbnail

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cryp/internal/crypto"
	"cryp/internal/session"
)

const (
	thumbDir      = "thumbnails"
	thumbWidth    = 320
	thumbHeight   = 180
	imageThumbMax = 1024
	queueSize     = 1000
	maxRetries    = 1
	failCooldown  = 5 * time.Minute
)

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
	VaultID   string
	VaultPath string
	Keys      *crypto.VaultKeys
	FilePath  string // virtual path within vault
}

// Generator manages async thumbnail generation with a single-worker queue.
// It generates thumbnails by having FFmpeg fetch the video via the local
// HTTP server's Range-enabled content endpoint. This means:
//   - No plaintext temp files — decryption happens on-the-fly via HTTP Range
//   - MP4 moov-at-end works — FFmpeg sends Range requests to seek
//   - Only a few MB of data is transferred for a single frame extraction
type Generator struct {
	vaultDir string
	sessions *session.Store
	port     string
	jobs     chan thumbJob
	ffmpeg   ffmpegConfig
	wg       sync.WaitGroup
	mu       sync.Mutex
	queued   map[string]struct{}
	failed   map[string]time.Time
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
		bin:     "ffmpeg",
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

	g := &Generator{
		vaultDir: vaultDir,
		sessions: sessions,
		port:     port,
		jobs:     make(chan thumbJob, queueSize),
		ffmpeg:   cfg,
		queued:   make(map[string]struct{}),
		failed:   make(map[string]time.Time),
	}
	g.wg.Add(1)
	go g.worker()
	return g
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
	out, err := exec.Command(bin, "-hide_banner", "-hwaccels").CombinedOutput()
	if err != nil {
		return map[string]bool{}
	}

	available := make(map[string]bool)
	lines := strings.Split(string(out), "\n")
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
	probePath := "/tmp/cryp-ffmpeg-probe.mp4"
	createCmd := exec.Command(
		bin,
		"-hide_banner",
		"-loglevel", "error",
		"-f", "lavfi",
		"-i", "testsrc2=size=128x72:rate=30",
		"-t", "1",
		"-c:v", "h264",
		"-y", probePath,
	)
	if out, err := createCmd.CombinedOutput(); err != nil {
		_ = out
		return false
	}

	probeArgs := make([]string, 0, 16)
	probeArgs = append(probeArgs, "-hide_banner", "-loglevel", "error")
	probeArgs = appendAttemptHwArgs(probeArgs, attempt)
	probeArgs = append(probeArgs, "-i", probePath, "-frames:v", "1", "-f", "null", "-")

	runCmd := exec.Command(bin, probeArgs...)
	out, err := runCmd.CombinedOutput()
	if err != nil {
		_ = out
		return false
	}
	return true
}

func appendAttemptHwArgs(args []string, attempt ffmpegAttempt) []string {
	if attempt.hwaccel == "" {
		return args
	}
	args = append(args, "-hwaccel", attempt.hwaccel)
	if attempt.hwaccel == "vaapi" {
		if _, err := os.Stat("/dev/dri/renderD128"); err == nil {
			args = append(args, "-hwaccel_device", "/dev/dri/renderD128")
		}
	}
	if attempt.hwaccelOutputFormat != "" {
		args = append(args, "-hwaccel_output_format", attempt.hwaccelOutputFormat)
	}
	return args
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

// Stop gracefully shuts down the generator
func (g *Generator) Stop() {
	close(g.jobs)
	g.wg.Wait()
}

// Enqueue adds a thumbnail generation job to the queue.
// Non-blocking: drops the job if queue is full.
func (g *Generator) Enqueue(vaultID, vaultPath string, keys *crypto.VaultKeys, virtualPath string) {
	if g.HasThumbnail(vaultID, virtualPath) {
		return
	}

	key := thumbJobKey(vaultID, virtualPath)
	now := time.Now()
	g.mu.Lock()
	if _, ok := g.queued[key]; ok {
		g.mu.Unlock()
		return
	}
	if failedUntil, ok := g.failed[key]; ok {
		if now.Before(failedUntil) {
			g.mu.Unlock()
			return
		}
		delete(g.failed, key)
	}
	g.queued[key] = struct{}{}
	g.mu.Unlock()

	job := thumbJob{
		VaultID:   vaultID,
		VaultPath: vaultPath,
		Keys:      keys,
		FilePath:  virtualPath,
	}
	select {
	case g.jobs <- job:
	default:
		g.mu.Lock()
		delete(g.queued, key)
		g.mu.Unlock()
		// Queue full, skip this thumbnail.
	}
}

// GetPath returns the thumbnail file path if it exists, or empty string
func (g *Generator) GetPath(vaultID, virtualPath string) string {
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
	p := g.thumbPath(vaultID, virtualPath)
	os.Remove(p) // ignore error if not exists
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
		key := thumbJobKey(job.VaultID, job.FilePath)
		finish := func(err error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.queued, key)
			if err != nil {
				g.failed[key] = time.Now().Add(failCooldown)
				return
			}
			delete(g.failed, key)
		}

		// Skip if already exists
		if g.HasThumbnail(job.VaultID, job.FilePath) {
			finish(nil)
			continue
		}
		if err := g.generate(job); err != nil {
			finish(err)
			log.Printf("thumbnail: failed to generate for %s: %v", job.FilePath, err)
			continue
		}
		finish(nil)
	}
}

// generate creates a thumbnail by having FFmpeg fetch the video via the local
// HTTP server. The server supports Range requests, so FFmpeg can seek to read
// the moov atom (even at end of file) and extract a frame — all without any
// plaintext ever touching disk.
func (g *Generator) generate(job thumbJob) error {
	if IsHEIF(job.FilePath) {
		return g.generateHEIF(job)
	}

	keysCopy := job.Keys.Clone()

	// Create a short-lived internal session so FFmpeg can authenticate
	sessionID, err := g.sessions.Create(job.VaultID, job.VaultPath, keysCopy)
	if err != nil {
		return fmt.Errorf("create temp session: %w", err)
	}
	defer g.sessions.Delete(sessionID)

	// Build the internal URL for the content endpoint
	contentURL := fmt.Sprintf("http://127.0.0.1:%s/api/vaults/%s/files/content?path=%s",
		g.port, job.VaultID, url.QueryEscape(job.FilePath))

	// Ensure thumbnail directory exists
	outPath := g.thumbPath(job.VaultID, job.FilePath)
	if err := os.MkdirAll(filepath.Dir(outPath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	thumbTmp := outPath + ".tmp"
	defer os.Remove(thumbTmp)

	var lastErr error
	for _, attempt := range g.ffmpeg.attempts {
		ffmpegArgs := make([]string, 0, 24)
		ffmpegArgs = appendAttemptHwArgs(ffmpegArgs, attempt)

		ffmpegArgs = append(ffmpegArgs,
			// Pass session cookie via HTTP headers for authentication
			"-headers", fmt.Sprintf("Cookie: session_id=%s\r\n", sessionID),
			// Input from local HTTP server (supports Range for seeking)
			"-i", contentURL,
			// Extract a single frame
			"-frames:v", "1",
			// Scale to thumbnail size with padding to maintain aspect ratio
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black",
				thumbWidth, thumbHeight, thumbWidth, thumbHeight),
			"-q:v", "5",
			"-f", "image2",
			"-y", thumbTmp,
		)

		var stderrBuf bytes.Buffer
		cmd := exec.Command(g.ffmpeg.bin, ffmpegArgs...)
		cmd.Stderr = &stderrBuf

		if err := cmd.Run(); err != nil {
			errOut := stderrBuf.String()
			if len(errOut) > 500 {
				errOut = errOut[len(errOut)-500:]
			}

			if attempt.hwaccel != "" {
				log.Printf("thumbnail: hwaccel=%s failed, fallback next backend: %v", attempt.hwaccel, err)
			}
			lastErr = fmt.Errorf("ffmpeg: %w: %s", err, errOut)
			continue
		}

		lastErr = nil
		break
	}

	if lastErr != nil {
		return lastErr
	}

	// Encrypt the thumbnail JPEG before storing
	if err := g.encryptFile(thumbTmp, outPath, job.Keys.MasterKey); err != nil {
		return fmt.Errorf("encrypt thumbnail: %w", err)
	}

	return nil
}

func (g *Generator) generateHEIF(job thumbJob) error {
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
	thumbTmp := outPath + ".tmp"
	defer os.Remove(thumbTmp)

	if err := g.writeDecryptedFile(job, heifPath); err != nil {
		return fmt.Errorf("decrypt heif: %w", err)
	}

	var heifErr bytes.Buffer
	convertCmd := exec.Command("heif-convert", heifPath, fullJPEGPath)
	convertCmd.Stderr = &heifErr
	if err := convertCmd.Run(); err != nil {
		errOut := heifErr.String()
		if len(errOut) > 500 {
			errOut = errOut[len(errOut)-500:]
		}
		return fmt.Errorf("heif-convert: %w: %s", err, errOut)
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

	var ffmpegErr bytes.Buffer
	cmd := exec.Command(g.ffmpeg.bin, ffmpegArgs...)
	cmd.Stderr = &ffmpegErr
	if err := cmd.Run(); err != nil {
		errOut := ffmpegErr.String()
		if len(errOut) > 500 {
			errOut = errOut[len(errOut)-500:]
		}
		return fmt.Errorf("ffmpeg scale heif: %w: %s", err, errOut)
	}

	if err := g.encryptFile(thumbTmp, outPath, job.Keys.MasterKey); err != nil {
		return fmt.Errorf("encrypt thumbnail: %w", err)
	}
	return nil
}

func (g *Generator) writeDecryptedFile(job thumbJob, outPath string) error {
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
	if _, err := io.CopyBuffer(out, reader, *bufp); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}

	crypto.DropFileCache(in)
	crypto.DropFileCache(out)
	return nil
}

// ScanVault scans a vault for video files that are missing thumbnails and enqueues them.
// Runs in a goroutine to avoid blocking startup.
func (g *Generator) ScanVault(vaultID, vaultPath string, keys *crypto.VaultKeys) {
	go func() {
		vault := &crypto.Vault{
			ID:   vaultID,
			Path: vaultPath,
			Keys: keys,
		}
		g.scanDir(vault, "/")
	}()
}

func (g *Generator) scanDir(vault *crypto.Vault, dirPath string) {
	files, err := vault.ListDirectory(dirPath)
	if err != nil {
		return
	}

	for _, f := range files {
		fullPath := dirPath
		if fullPath == "/" {
			fullPath = "/" + f.Name
		} else {
			fullPath = dirPath + "/" + f.Name
		}

		if f.IsDir {
			g.scanDir(vault, fullPath)
		} else if (IsVideo(f.Name) || IsHEIF(f.Name)) && !g.HasThumbnail(vault.ID, fullPath) {
			g.Enqueue(vault.ID, vault.Path, vault.Keys, fullPath)
		}
	}
}

// encryptFile reads a plaintext file, encrypts it using the same format as vault
// content files (header + AES-256-GCM chunks), and writes the result to outPath.
func (g *Generator) encryptFile(srcPath, outPath string, masterKey []byte) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	contentKey := make([]byte, crypto.MasterKeySize)
	if _, err := rand.Read(contentKey); err != nil {
		os.Remove(outPath)
		return err
	}

	header, err := crypto.WriteFileHeader(out, masterKey, contentKey)
	if err != nil {
		os.Remove(outPath)
		return err
	}

	writer, err := crypto.NewEncryptingWriter(out, header.ContentKey, header.Nonce)
	if err != nil {
		os.Remove(outPath)
		return err
	}

	if _, err := crypto.CopyWithCacheDrop(writer, src, out); err != nil {
		os.Remove(outPath)
		return err
	}

	if err := writer.Close(); err != nil {
		os.Remove(outPath)
		return err
	}

	if err := out.Sync(); err != nil {
		os.Remove(outPath)
		return err
	}

	crypto.DropFileCache(src)
	crypto.DropFileCache(out)

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
