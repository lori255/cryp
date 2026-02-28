package crypto

import (
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"golang.org/x/sys/unix"
)

const (
	cacheDropInterval = 8 * 1024 * 1024 // 8MB - keep low to limit page cache in containers
	copyBufSize       = 32 * 1024        // 32KB
)

// CopyWithCacheDrop copies src to dst through encWriter, periodically
// dropping page cache on both files to limit memory usage during large file encryption.
func CopyWithCacheDrop(encWriter io.Writer, src *os.File, dst *os.File) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	var sinceLastDrop int64
	var srcDropped int64

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			nw, writeErr := encWriter.Write(buf[:n])
			total += int64(nw)
			sinceLastDrop += int64(nw)
			if writeErr != nil {
				return total, writeErr
			}
			if sinceLastDrop >= cacheDropInterval {
				unix.Fadvise(int(src.Fd()), srcDropped, sinceLastDrop, unix.FADV_DONTNEED)
				srcDropped += sinceLastDrop
				dst.Sync()
				pos, _ := dst.Seek(0, io.SeekCurrent)
				if pos > 0 {
					unix.Fadvise(int(dst.Fd()), 0, pos, unix.FADV_DONTNEED)
				}
				sinceLastDrop = 0
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// StreamCopyWithOutputCacheDrop copies from an io.Reader (e.g. multipart body)
// through encWriter to disk, periodically dropping page cache on the output file.
// Unlike CopyWithCacheDrop, this does NOT fadvise the source since network
// streams have no page cache.
func StreamCopyWithOutputCacheDrop(encWriter io.Writer, src io.Reader, dst *os.File) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	var sinceLastDrop int64

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			nw, writeErr := encWriter.Write(buf[:n])
			total += int64(nw)
			sinceLastDrop += int64(nw)
			if writeErr != nil {
				return total, writeErr
			}
			if sinceLastDrop >= cacheDropInterval {
				dst.Sync()
				pos, _ := dst.Seek(0, io.SeekCurrent)
				if pos > 0 {
					unix.Fadvise(int(dst.Fd()), 0, pos, unix.FADV_DONTNEED)
				}
				sinceLastDrop = 0
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// DropFileCache syncs and drops all page cache for the file.
func DropFileCache(f *os.File) {
	if err := f.Sync(); err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		return
	}
	unix.Fadvise(int(f.Fd()), 0, info.Size(), unix.FADV_DONTNEED)
}

// EncryptSingleFile encrypts a file from srcPath into the vault at virtualPath.
func EncryptSingleFile(vault *Vault, keys *VaultKeys, srcPath, virtualPath string) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	encPath, err := vault.GetEncryptedFilePath(virtualPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(encPath), 0700); err != nil {
		return err
	}

	outFile, err := os.Create(encPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	unix.Fadvise(int(srcFile.Fd()), 0, 0, unix.FADV_SEQUENTIAL|unix.FADV_NOREUSE)

	contentKey := make([]byte, MasterKeySize)
	if _, err := rand.Read(contentKey); err != nil {
		return err
	}

	header, err := WriteFileHeader(outFile, keys.MasterKey, contentKey)
	if err != nil {
		os.Remove(encPath)
		return err
	}

	writer, err := NewEncryptingWriter(outFile, header.ContentKey, header.Nonce)
	if err != nil {
		os.Remove(encPath)
		return err
	}

	if _, err := CopyWithCacheDrop(writer, srcFile, outFile); err != nil {
		os.Remove(encPath)
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	DropFileCache(srcFile)
	DropFileCache(outFile)
	return nil
}

// ReleaseMemoryAfterLargeFile forces GC and returns memory to OS.
// Call after processing files > 100MB.
func ReleaseMemoryAfterLargeFile() {
	runtime.GC()
	debug.FreeOSMemory()
}

// DropPathCache opens a file by path and drops its page cache.
// Useful after the original fd is closed but cache persists.
func DropPathCache(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return
	}
	unix.Fadvise(int(f.Fd()), 0, info.Size(), unix.FADV_DONTNEED)
	f.Close()
}
