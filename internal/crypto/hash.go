package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// HashVirtualFile returns the SHA-256 hash of a decrypted vault file.
func (v *Vault) HashVirtualFile(virtualPath string) (string, error) {
	encPath, err := v.ResolveExistingFilePath(virtualPath)
	if err != nil {
		return "", err
	}

	file, err := os.Open(encPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	header, err := ReadFileHeader(file, v.Keys.MasterKey)
	if err != nil {
		return "", fmt.Errorf("read file header: %w", err)
	}

	reader, err := NewDecryptingReader(file, header.ContentKey, header.Nonce)
	if err != nil {
		return "", fmt.Errorf("create decrypting reader: %w", err)
	}
	defer reader.Release()

	hasher := sha256.New()
	bufp := CopyBufPool.Get().(*[]byte)
	defer CopyBufPool.Put(bufp)
	if _, err := io.CopyBuffer(hasher, reader, *bufp); err != nil {
		return "", fmt.Errorf("hash decrypted content: %w", err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}
