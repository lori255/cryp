package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/scrypt"
)

const (
	// ScryptN is the CPU/memory cost parameter (2^15 = 32768)
	ScryptN = 32768
	// ScryptR is the block size parameter
	ScryptR = 8
	// ScryptP is the parallelization parameter
	ScryptP = 1
	// SaltSize is the size of scrypt salt in bytes
	SaltSize = 16
	// MasterKeySize is 256-bit master key
	MasterKeySize = 32
	// MACKeySize is 256-bit MAC key
	MACKeySize = 32
	// WrappedKeySize is the size after AES Key Wrap (adds 8 bytes)
	WrappedKeySize = MasterKeySize + 8
)

// VaultKeys holds the unwrapped vault keys
type VaultKeys struct {
	MasterKey []byte // 32 bytes - used for content encryption
	MACKey    []byte // 32 bytes - used for filename encryption (AES-SIV)
}

// VaultConfig holds the encrypted vault configuration stored in vault.json
type VaultConfig struct {
	ScryptSalt       []byte `json:"scryptSalt"`       // 16 bytes
	ScryptN          int    `json:"scryptN"`           // 32768
	ScryptR          int    `json:"scryptR"`           // 8
	ScryptP          int    `json:"scryptP"`           // 1
	WrappedMasterKey []byte `json:"wrappedMasterKey"`  // 40 bytes (32 + 8 wrap overhead)
	WrappedMACKey    []byte `json:"wrappedMACKey"`     // 40 bytes
}

// DeriveKEK derives a Key Encryption Key from password using scrypt
func DeriveKEK(password []byte, salt []byte) ([]byte, error) {
	if len(salt) != SaltSize {
		return nil, fmt.Errorf("invalid salt size: expected %d, got %d", SaltSize, len(salt))
	}
	return scrypt.Key(password, salt, ScryptN, ScryptR, ScryptP, MasterKeySize)
}

// GenerateVaultKeys creates new random master and MAC keys
func GenerateVaultKeys() (*VaultKeys, error) {
	masterKey := make([]byte, MasterKeySize)
	if _, err := rand.Read(masterKey); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}

	macKey := make([]byte, MACKeySize)
	if _, err := rand.Read(macKey); err != nil {
		return nil, fmt.Errorf("generate mac key: %w", err)
	}

	return &VaultKeys{
		MasterKey: masterKey,
		MACKey:    macKey,
	}, nil
}

// CreateVaultConfig generates a new vault configuration with the given password
func CreateVaultConfig(password []byte) (*VaultConfig, *VaultKeys, error) {
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("generate salt: %w", err)
	}

	kek, err := DeriveKEK(password, salt)
	if err != nil {
		return nil, nil, fmt.Errorf("derive KEK: %w", err)
	}

	keys, err := GenerateVaultKeys()
	if err != nil {
		return nil, nil, fmt.Errorf("generate vault keys: %w", err)
	}

	wrappedMaster, err := AESKeyWrap(kek, keys.MasterKey)
	if err != nil {
		return nil, nil, fmt.Errorf("wrap master key: %w", err)
	}

	wrappedMAC, err := AESKeyWrap(kek, keys.MACKey)
	if err != nil {
		return nil, nil, fmt.Errorf("wrap mac key: %w", err)
	}

	config := &VaultConfig{
		ScryptSalt:       salt,
		ScryptN:          ScryptN,
		ScryptR:          ScryptR,
		ScryptP:          ScryptP,
		WrappedMasterKey: wrappedMaster,
		WrappedMACKey:    wrappedMAC,
	}

	return config, keys, nil
}

// UnlockVault derives keys from password and unwraps the vault keys
func UnlockVault(password []byte, config *VaultConfig) (*VaultKeys, error) {
	kek, err := scrypt.Key(password, config.ScryptSalt, config.ScryptN, config.ScryptR, config.ScryptP, MasterKeySize)
	if err != nil {
		return nil, fmt.Errorf("derive KEK: %w", err)
	}

	masterKey, err := AESKeyUnwrap(kek, config.WrappedMasterKey)
	if err != nil {
		return nil, fmt.Errorf("unwrap master key (wrong password?): %w", err)
	}

	macKey, err := AESKeyUnwrap(kek, config.WrappedMACKey)
	if err != nil {
		return nil, fmt.Errorf("unwrap mac key: %w", err)
	}

	return &VaultKeys{
		MasterKey: masterKey,
		MACKey:    macKey,
	}, nil
}

// DeriveContentKey derives a per-file content key from the master key and a file-specific nonce
func DeriveContentKey(masterKey []byte, headerNonce []byte) []byte {
	h := sha256.New()
	h.Write(masterKey)
	h.Write(headerNonce)
	return h.Sum(nil)
}

// --- AES Key Wrap (RFC 3394) ---

var defaultIV = []byte{0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6}

// AESKeyWrap wraps a key using AES Key Wrap (RFC 3394)
func AESKeyWrap(kek, plaintext []byte) ([]byte, error) {
	if len(plaintext)%8 != 0 {
		return nil, errors.New("plaintext must be multiple of 8 bytes")
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}

	n := len(plaintext) / 8
	// Initialize
	a := make([]byte, 8)
	copy(a, defaultIV)

	r := make([][]byte, n)
	for i := 0; i < n; i++ {
		r[i] = make([]byte, 8)
		copy(r[i], plaintext[i*8:(i+1)*8])
	}

	// Wrap
	for j := 0; j <= 5; j++ {
		for i := 1; i <= n; i++ {
			// B = AES(K, A | R[i])
			buf := make([]byte, 16)
			copy(buf[:8], a)
			copy(buf[8:], r[i-1])
			block.Encrypt(buf, buf)

			// A = MSB(64, B) ^ t where t = (n*j)+i
			copy(a, buf[:8])
			t := uint64(n*j + i)
			tBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(tBytes, t)
			for k := 0; k < 8; k++ {
				a[k] ^= tBytes[k]
			}
			// R[i] = LSB(64, B)
			copy(r[i-1], buf[8:])
		}
	}

	// Output C = A | R[1] | R[2] | ... | R[n]
	out := make([]byte, (n+1)*8)
	copy(out[:8], a)
	for i := 0; i < n; i++ {
		copy(out[(i+1)*8:(i+2)*8], r[i])
	}
	return out, nil
}

// AESKeyUnwrap unwraps a key using AES Key Unwrap (RFC 3394)
func AESKeyUnwrap(kek, ciphertext []byte) ([]byte, error) {
	if len(ciphertext)%8 != 0 || len(ciphertext) < 24 {
		return nil, errors.New("invalid ciphertext length")
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}

	n := (len(ciphertext) / 8) - 1

	a := make([]byte, 8)
	copy(a, ciphertext[:8])

	r := make([][]byte, n)
	for i := 0; i < n; i++ {
		r[i] = make([]byte, 8)
		copy(r[i], ciphertext[(i+1)*8:(i+2)*8])
	}

	// Unwrap
	for j := 5; j >= 0; j-- {
		for i := n; i >= 1; i-- {
			// A ^ t
			t := uint64(n*j + i)
			tBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(tBytes, t)
			for k := 0; k < 8; k++ {
				a[k] ^= tBytes[k]
			}
			// B = AES-1(K, (A ^ t) | R[i])
			buf := make([]byte, 16)
			copy(buf[:8], a)
			copy(buf[8:], r[i-1])
			block.Decrypt(buf, buf)

			copy(a, buf[:8])
			copy(r[i-1], buf[8:])
		}
	}

	// Verify integrity
	for i := 0; i < 8; i++ {
		if a[i] != defaultIV[i] {
			return nil, errors.New("integrity check failed: wrong key or corrupted data")
		}
	}

	out := make([]byte, n*8)
	for i := 0; i < n; i++ {
		copy(out[i*8:(i+1)*8], r[i])
	}
	return out, nil
}

// GenerateNonce generates a random nonce of the specified size
func GenerateNonce(size int) ([]byte, error) {
	nonce := make([]byte, size)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return nonce, nil
}

// NewGCM creates a new AES-GCM cipher
func NewGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
