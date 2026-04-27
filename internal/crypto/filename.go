package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// AES-SIV (RFC 5297) implementation for deterministic filename encryption.
// Uses two 256-bit keys: one for S2V (MAC) and one for CTR encryption.

const (
	sivBlockSize = aes.BlockSize // 16 bytes
)

// EncryptFileName encrypts a filename using AES-SIV with the directory ID as associated data
func EncryptFileName(macKey []byte, name string, dirID string) (string, error) {
	if len(macKey) != 32 {
		return "", fmt.Errorf("macKey must be 32 bytes, got %d", len(macKey))
	}
	if name == "" {
		return "", errors.New("filename cannot be empty")
	}

	// Derive two 256-bit keys from the MAC key for SIV
	// Key1 = first 16 bytes for S2V, Key2 = last 16 bytes for CTR
	sivKey := deriveSIVKeys(macKey)

	plaintext := []byte(name)
	ad := []byte(dirID)

	ciphertext, err := sivEncrypt(sivKey, plaintext, ad)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// DecryptFileName decrypts a filename using AES-SIV with the directory ID as associated data
func DecryptFileName(macKey []byte, encryptedName string, dirID string) (string, error) {
	if len(macKey) != 32 {
		return "", fmt.Errorf("macKey must be 32 bytes, got %d", len(macKey))
	}

	ciphertext, err := base64.RawURLEncoding.DecodeString(encryptedName)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}

	sivKey := deriveSIVKeys(macKey)
	ad := []byte(dirID)

	plaintext, err := sivDecrypt(sivKey, ciphertext, ad)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}

// EncryptIndexPath encrypts a plaintext virtual path before it is stored in
// SQLite metadata. It is deterministic so the encrypted value can be used as a
// stable primary key, but it cannot be read without the vault MAC key.
func EncryptIndexPath(macKey []byte, vaultID, virtualPath string) (string, error) {
	return EncryptFileName(macKey, normalizeIndexPath(virtualPath), indexPathAAD(vaultID))
}

// DecryptIndexPath decrypts a virtual path stored by EncryptIndexPath.
func DecryptIndexPath(macKey []byte, vaultID, encryptedPath string) (string, error) {
	return DecryptFileName(macKey, encryptedPath, indexPathAAD(vaultID))
}

func EncryptEntryNameKey(macKey []byte, vaultID, parentKey, name string) (string, error) {
	if name == "" {
		name = "/"
	}
	return EncryptFileName(macKey, name, "cryp:entry-name:v1:"+vaultID+":"+parentKey)
}

func NormalizeVirtualPath(virtualPath string) string {
	return normalizeIndexPath(virtualPath)
}

func ParentVirtualPath(virtualPath string) string {
	normalized := normalizeIndexPath(virtualPath)
	if normalized == "/" {
		return ""
	}
	parent := filepath.Dir(normalized)
	if parent == "." {
		return "/"
	}
	return parent
}

func BaseVirtualName(virtualPath string) string {
	normalized := normalizeIndexPath(virtualPath)
	if normalized == "/" {
		return "/"
	}
	return filepath.Base(normalized)
}

func indexPathAAD(vaultID string) string {
	return "cryp:file-index-path:v1:" + vaultID
}

func normalizeIndexPath(virtualPath string) string {
	cleaned := filepath.Clean(virtualPath)
	if cleaned == "." {
		return "/"
	}
	if !strings.HasPrefix(cleaned, "/") {
		return "/" + cleaned
	}
	return cleaned
}

// sivKeys holds the two AES keys used in SIV mode
type sivKeys struct {
	macKey []byte // 16 bytes for CMAC
	encKey []byte // 16 bytes for CTR
}

func deriveSIVKeys(key []byte) *sivKeys {
	return &sivKeys{
		macKey: key[:16],
		encKey: key[16:],
	}
}

// sivEncrypt performs AES-SIV encryption
func sivEncrypt(keys *sivKeys, plaintext, ad []byte) ([]byte, error) {
	// Step 1: Compute SIV tag using S2V
	siv, err := s2v(keys.macKey, plaintext, ad)
	if err != nil {
		return nil, err
	}

	// Step 2: Encrypt plaintext using AES-CTR with SIV as IV
	ctrIV := make([]byte, sivBlockSize)
	copy(ctrIV, siv)
	// Clear bits 31 and 63 for CTR mode (per RFC 5297)
	ctrIV[8] &= 0x7F
	ctrIV[12] &= 0x7F

	block, err := aes.NewCipher(keys.encKey)
	if err != nil {
		return nil, err
	}
	ctr := cipher.NewCTR(block, ctrIV)
	ciphertext := make([]byte, len(plaintext))
	ctr.XORKeyStream(ciphertext, plaintext)

	// Output: SIV(16) || ciphertext
	out := make([]byte, sivBlockSize+len(ciphertext))
	copy(out[:sivBlockSize], siv)
	copy(out[sivBlockSize:], ciphertext)
	return out, nil
}

// sivDecrypt performs AES-SIV decryption
func sivDecrypt(keys *sivKeys, data, ad []byte) ([]byte, error) {
	if len(data) < sivBlockSize {
		return nil, errors.New("ciphertext too short for SIV")
	}

	siv := data[:sivBlockSize]
	ciphertext := data[sivBlockSize:]

	// Decrypt using AES-CTR with SIV as IV
	ctrIV := make([]byte, sivBlockSize)
	copy(ctrIV, siv)
	ctrIV[8] &= 0x7F
	ctrIV[12] &= 0x7F

	block, err := aes.NewCipher(keys.encKey)
	if err != nil {
		return nil, err
	}
	ctr := cipher.NewCTR(block, ctrIV)
	plaintext := make([]byte, len(ciphertext))
	ctr.XORKeyStream(plaintext, ciphertext)

	// Verify SIV tag
	computedSIV, err := s2v(keys.macKey, plaintext, ad)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare(siv, computedSIV) != 1 {
		return nil, errors.New("SIV authentication failed")
	}

	return plaintext, nil
}

// s2v computes the S2V function per RFC 5297
func s2v(macKey []byte, plaintext, ad []byte) ([]byte, error) {
	block, err := aes.NewCipher(macKey)
	if err != nil {
		return nil, err
	}

	// D = AES-CMAC(K, <zero>)
	zero := make([]byte, sivBlockSize)
	d := aesCMAC(block, zero)

	// D = dbl(D) xor AES-CMAC(K, ad)
	d = dbl(d)
	t := aesCMAC(block, ad)
	xorBytes(d, t)

	// Final: if len(plaintext) >= 16, xorend; else pad and xor
	var lastInput []byte
	if len(plaintext) >= sivBlockSize {
		lastInput = make([]byte, len(plaintext))
		copy(lastInput, plaintext)
		// XOR D into the last 16 bytes
		for i := 0; i < sivBlockSize; i++ {
			lastInput[len(plaintext)-sivBlockSize+i] ^= d[i]
		}
	} else {
		// Pad plaintext and XOR with doubled D
		padded := pad(plaintext)
		d = dbl(d)
		lastInput = make([]byte, sivBlockSize)
		copy(lastInput, padded)
		xorBytes(lastInput, d)
	}

	return aesCMAC(block, lastInput), nil
}

// aesCMAC computes AES-CMAC (simplified for SIV use)
func aesCMAC(block cipher.Block, data []byte) []byte {
	k1, k2 := generateSubkeys(block)

	n := (len(data) + sivBlockSize - 1) / sivBlockSize
	if n == 0 {
		n = 1
	}

	// Check if last block is complete
	lastBlockComplete := (len(data) > 0) && (len(data)%sivBlockSize == 0)

	x := make([]byte, sivBlockSize)
	for i := 0; i < n-1; i++ {
		y := make([]byte, sivBlockSize)
		copy(y, data[i*sivBlockSize:])
		xorBytes(y, x)
		block.Encrypt(x, y)
	}

	// Process last block
	lastBlock := make([]byte, sivBlockSize)
	if lastBlockComplete {
		start := (n - 1) * sivBlockSize
		copy(lastBlock, data[start:])
		xorBytes(lastBlock, k1)
	} else {
		start := (n - 1) * sivBlockSize
		remaining := len(data) - start
		if remaining > 0 {
			copy(lastBlock, data[start:])
		}
		lastBlock[remaining] = 0x80
		xorBytes(lastBlock, k2)
	}

	xorBytes(lastBlock, x)
	mac := make([]byte, sivBlockSize)
	block.Encrypt(mac, lastBlock)
	return mac
}

func generateSubkeys(block cipher.Block) (k1, k2 []byte) {
	l := make([]byte, sivBlockSize)
	block.Encrypt(l, l)

	k1 = dbl(l)
	k2 = dbl(k1)
	return
}

func dbl(data []byte) []byte {
	out := make([]byte, len(data))
	carry := byte(0)
	for i := len(data) - 1; i >= 0; i-- {
		out[i] = (data[i] << 1) | carry
		carry = data[i] >> 7
	}
	if data[0]&0x80 != 0 {
		out[len(out)-1] ^= 0x87
	}
	return out
}

func pad(data []byte) []byte {
	padded := make([]byte, sivBlockSize)
	copy(padded, data)
	padded[len(data)] = 0x80
	return padded
}

func xorBytes(dst, src []byte) {
	for i := 0; i < len(dst) && i < len(src); i++ {
		dst[i] ^= src[i]
	}
}
