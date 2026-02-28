package thumbnail

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"cryp/internal/crypto"
)

const (
	thumbDir    = "thumbnails"
	thumbWidth  = 320
	thumbHeight = 180
	queueSize   = 1000
	maxRetries  = 1
)

// videoExtensions lists supported video file extensions
var videoExtensions = map[string]bool{
	".mp4": true, ".webm": true, ".mkv": true, ".avi": true,
	".mov": true, ".m4v": true, ".flv": true, ".wmv": true,
}

// IsVideo checks if a filename has a video extension
func IsVideo(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return videoExtensions[ext]
}

// thumbJob represents a thumbnail generation request
type thumbJob struct {
	VaultID   string
	VaultPath string
	Keys      *crypto.VaultKeys
	FilePath  string // virtual path within vault
}

// Generator manages async thumbnail generation with a single-worker queue
type Generator struct {
	vaultDir string
	jobs     chan thumbJob
	wg       sync.WaitGroup
}

// NewGenerator creates a new thumbnail generator
func NewGenerator(vaultDir string) *Generator {
	g := &Generator{
		vaultDir: vaultDir,
		jobs:     make(chan thumbJob, queueSize),
	}
	g.wg.Add(1)
	go g.worker()
	return g
}

// Stop gracefully shuts down the generator
func (g *Generator) Stop() {
	close(g.jobs)
	g.wg.Wait()
}

// Enqueue adds a thumbnail generation job to the queue.
// Non-blocking: drops the job if queue is full.
func (g *Generator) Enqueue(vaultID, vaultPath string, keys *crypto.VaultKeys, virtualPath string) {
	select {
	case g.jobs <- thumbJob{
		VaultID:   vaultID,
		VaultPath: vaultPath,
		Keys:      keys,
		FilePath:  virtualPath,
	}:
	default:
		// Queue full, skip this thumbnail
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

// thumbPath returns the on-disk path for a thumbnail
func (g *Generator) thumbPath(vaultID, virtualPath string) string {
	hash := sha256.Sum256([]byte(virtualPath))
	name := fmt.Sprintf("%x.c9r", hash)
	return filepath.Join(g.vaultDir, vaultID, thumbDir, name)
}

// worker processes thumbnail generation jobs sequentially
func (g *Generator) worker() {
	defer g.wg.Done()
	for job := range g.jobs {
		// Skip if already exists
		if g.HasThumbnail(job.VaultID, job.FilePath) {
			continue
		}
		if err := g.generate(job); err != nil {
			log.Printf("thumbnail: failed to generate for %s: %v", job.FilePath, err)
		}
	}
}

// generate creates a thumbnail for a single video file
func (g *Generator) generate(job thumbJob) error {
	vault := &crypto.Vault{
		ID:   job.VaultID,
		Path: job.VaultPath,
		Keys: job.Keys,
	}

	// Resolve encrypted file path
	encPath, err := vault.GetEncryptedFilePath(job.FilePath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// Open encrypted file
	file, err := os.Open(encPath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	// Read file header to get content key
	header, err := crypto.ReadFileHeader(file, job.Keys.MasterKey)
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	// Create decrypting reader
	reader, err := crypto.NewDecryptingReader(file, header.ContentKey, header.Nonce)
	if err != nil {
		return fmt.Errorf("create reader: %w", err)
	}
	// reader.Release() called explicitly after copy to temp file

	// Ensure thumbnail directory exists
	outPath := g.thumbPath(job.VaultID, job.FilePath)
	if err := os.MkdirAll(filepath.Dir(outPath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// Decrypt entire file to temp file so FFmpeg can seek (MP4 moov atom may be at EOF)
	decryptedTmp, err := os.CreateTemp(filepath.Dir(outPath), "dec-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	decryptedPath := decryptedTmp.Name()
	defer os.Remove(decryptedPath)

	if _, err := io.CopyBuffer(decryptedTmp, reader, make([]byte, 256*1024)); err != nil {
		decryptedTmp.Close()
		return fmt.Errorf("decrypt to temp: %w", err)
	}
	decryptedTmp.Close()
	reader.Release() // release crypto resources early

	// Run FFmpeg: extract first frame from decrypted temp file
	thumbTmp := outPath + ".tmp"
	defer os.Remove(thumbTmp)

	var stderrBuf bytes.Buffer
	cmd := exec.Command("ffmpeg",
		"-i", decryptedPath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black",
			thumbWidth, thumbHeight, thumbWidth, thumbHeight),
		"-q:v", "5",
		"-f", "image2",
		"-y", thumbTmp,
	)
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		errOut := stderrBuf.String()
		if len(errOut) > 500 {
			errOut = errOut[len(errOut)-500:]
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, errOut)
	}

	// Encrypt the thumbnail JPEG before storing
	if err := g.encryptFile(thumbTmp, outPath, job.Keys.MasterKey); err != nil {
		return fmt.Errorf("encrypt thumbnail: %w", err)
	}

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
		} else if IsVideo(f.Name) && !g.HasThumbnail(vault.ID, fullPath) {
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

	if _, err := io.Copy(writer, src); err != nil {
		os.Remove(outPath)
		return err
	}

	if err := writer.Close(); err != nil {
		os.Remove(outPath)
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
