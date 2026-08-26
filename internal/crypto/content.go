package crypto

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	// HeaderNonceSize is the nonce size for the file header
	HeaderNonceSize = 12
	// HeaderPayloadSize is reserved(8) + contentKey(32) = 40 bytes
	HeaderPayloadSize = 40
	// HeaderTagSize is the GCM authentication tag size
	HeaderTagSize = 16
	// HeaderSize is the total file header: nonce(12) + encrypted_payload(40) + tag(16) = 68
	HeaderSize = HeaderNonceSize + HeaderPayloadSize + HeaderTagSize

	// ChunkNonceSize is the nonce size for each content chunk
	ChunkNonceSize = 12
	// ChunkPayloadSize is the max plaintext size per chunk (32 KiB)
	ChunkPayloadSize = 32 * 1024
	// ChunkTagSize is the GCM tag size per chunk
	ChunkTagSize = 16
	// ChunkCipherSize is the max ciphertext chunk on disk: nonce(12) + ciphertext(32768) + tag(16)
	ChunkCipherSize = ChunkNonceSize + ChunkPayloadSize + ChunkTagSize

	// HeaderReserved is the 8-byte reserved field in the header
	HeaderReservedValue = 0xFFFFFFFFFFFFFFFF
)

// ErrCorruptContent is returned when an encrypted file contains a truncated
// or otherwise structurally invalid chunk. It is intentionally distinguishable
// from an authentication failure so HTTP callers can report a damaged file
// instead of a transient server/transcode error.
var ErrCorruptContent = errors.New("corrupt encrypted content")

// FileHeader represents the decrypted file header
type FileHeader struct {
	Nonce      []byte // 12 bytes - also used as AAD for chunks
	ContentKey []byte // 32 bytes - per-file encryption key
}

// WriteFileHeader encrypts and writes a file header
func WriteFileHeader(w io.Writer, masterKey []byte, contentKey []byte) (*FileHeader, error) {
	nonce, err := GenerateNonce(HeaderNonceSize)
	if err != nil {
		return nil, fmt.Errorf("generate header nonce: %w", err)
	}

	gcm, err := NewGCM(masterKey)
	if err != nil {
		return nil, fmt.Errorf("create header GCM: %w", err)
	}

	// Build plaintext payload: reserved(8) + contentKey(32)
	payload := make([]byte, HeaderPayloadSize)
	defer clear(payload)
	binary.BigEndian.PutUint64(payload[:8], HeaderReservedValue)
	copy(payload[8:], contentKey)

	// Encrypt header payload (no AAD for header)
	ciphertext := gcm.Seal(nil, nonce, payload, nil)

	// Write: nonce || ciphertext_with_tag
	if _, err := w.Write(nonce); err != nil {
		return nil, err
	}
	if _, err := w.Write(ciphertext); err != nil {
		return nil, err
	}

	return &FileHeader{
		Nonce:      nonce,
		ContentKey: contentKey,
	}, nil
}

// ReadFileHeader reads and decrypts a file header
func ReadFileHeader(r io.Reader, masterKey []byte) (*FileHeader, error) {
	headerBytes := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, headerBytes); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	nonce := headerBytes[:HeaderNonceSize]
	ciphertext := headerBytes[HeaderNonceSize:]

	gcm, err := NewGCM(masterKey)
	if err != nil {
		return nil, fmt.Errorf("create header GCM: %w", err)
	}

	payload, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt header: %w", err)
	}
	defer clear(payload)

	reserved := binary.BigEndian.Uint64(payload[:8])
	if reserved != HeaderReservedValue {
		return nil, errors.New("invalid header reserved field")
	}

	contentKey := make([]byte, MasterKeySize)
	copy(contentKey, payload[8:])

	return &FileHeader{
		Nonce:      nonce,
		ContentKey: contentKey,
	}, nil
}

// chunkAAD builds the Associated Authenticated Data for a chunk
// AAD = chunkNumber(8 bytes, big-endian) || headerNonce(12 bytes)
func chunkAAD(chunkNumber uint64, headerNonce []byte) []byte {
	aad := make([]byte, 8+len(headerNonce))
	fillAAD(aad, chunkNumber, headerNonce)
	return aad
}

// fillAAD writes AAD into a pre-allocated buffer (zero alloc)
func fillAAD(buf []byte, chunkNumber uint64, headerNonce []byte) {
	binary.BigEndian.PutUint64(buf[:8], chunkNumber)
	copy(buf[8:], headerNonce)
}

// EncryptChunk encrypts a single plaintext chunk
func EncryptChunk(gcm cipher.AEAD, plaintext []byte, chunkNumber uint64, headerNonce []byte) ([]byte, error) {
	nonce, err := GenerateNonce(ChunkNonceSize)
	if err != nil {
		return nil, err
	}

	aad := chunkAAD(chunkNumber, headerNonce)
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)

	// Output: nonce || ciphertext_with_tag
	out := make([]byte, ChunkNonceSize+len(ciphertext))
	copy(out[:ChunkNonceSize], nonce)
	copy(out[ChunkNonceSize:], ciphertext)
	return out, nil
}

// DecryptChunk decrypts a single ciphertext chunk
func DecryptChunk(gcm cipher.AEAD, chunkData []byte, chunkNumber uint64, headerNonce []byte) ([]byte, error) {
	if len(chunkData) < ChunkNonceSize+ChunkTagSize {
		return nil, fmt.Errorf("%w: chunk data too short", ErrCorruptContent)
	}

	nonce := chunkData[:ChunkNonceSize]
	ciphertext := chunkData[ChunkNonceSize:]
	aad := chunkAAD(chunkNumber, headerNonce)

	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: decrypt chunk %d: %w", ErrCorruptContent, chunkNumber, err)
	}
	return plaintext, nil
}

// EncryptingWriter encrypts data in chunks and writes to the underlying writer
type EncryptingWriter struct {
	w           io.Writer
	gcm         cipher.AEAD
	headerNonce []byte
	chunkNum    uint64
	buf         []byte
	outBuf      []byte // reusable buffer for encrypted chunk output
	aadBuf      []byte // reusable AAD buffer (8 + nonceLen)
}

// NewEncryptingWriter creates a writer that encrypts data in chunks
func NewEncryptingWriter(w io.Writer, contentKey []byte, headerNonce []byte) (*EncryptingWriter, error) {
	gcm, err := NewGCM(contentKey)
	if err != nil {
		return nil, err
	}

	return &EncryptingWriter{
		w:           w,
		gcm:         gcm,
		headerNonce: headerNonce,
		chunkNum:    0,
		buf:         make([]byte, 0, ChunkPayloadSize),
		outBuf:      make([]byte, ChunkCipherSize),
		aadBuf:      make([]byte, 8+len(headerNonce)),
	}, nil
}

func (ew *EncryptingWriter) Write(p []byte) (int, error) {
	totalWritten := 0

	for len(p) > 0 {
		space := ChunkPayloadSize - len(ew.buf)
		if len(p) < space {
			ew.buf = append(ew.buf, p...)
			totalWritten += len(p)
			break
		}

		ew.buf = append(ew.buf, p[:space]...)
		p = p[space:]
		totalWritten += space

		if err := ew.flushChunk(); err != nil {
			return totalWritten, err
		}
	}

	return totalWritten, nil
}

func (ew *EncryptingWriter) flushChunk() error {
	if len(ew.buf) == 0 {
		return nil
	}

	// Zero-alloc: write nonce directly into outBuf front 12 bytes
	nonce := ew.outBuf[:ChunkNonceSize]
	if _, err := rand.Read(nonce); err != nil {
		return err
	}

	fillAAD(ew.aadBuf, ew.chunkNum, ew.headerNonce)
	ciphertext := ew.gcm.Seal(ew.outBuf[ChunkNonceSize:ChunkNonceSize], nonce, ew.buf, ew.aadBuf)
	total := ChunkNonceSize + len(ciphertext)

	if _, err := ew.w.Write(ew.outBuf[:total]); err != nil {
		return err
	}

	ew.chunkNum++
	ew.buf = ew.buf[:0]
	return nil
}

// Close flushes any remaining buffered data
func (ew *EncryptingWriter) Close() error {
	return ew.flushChunk()
}

// DecryptingReader reads encrypted chunks and returns decrypted data
type DecryptingReader struct {
	r           io.Reader
	gcm         cipher.AEAD
	headerNonce []byte
	chunkNum    uint64
	buf         []byte // decrypted plaintext buffer
	chunkBuf    []byte // reusable read buffer for encrypted chunks
	ptBuf       []byte // reusable plaintext decryption buffer
	aadBuf      []byte // reusable AAD buffer (8 + nonceLen)
	offset      int
	done        bool
}

// decryptingReaderPool reuses DecryptingReader structs to avoid per-request allocation.
// Each reader contains ~65KB of buffers (chunkBuf + ptBuf + aadBuf).
var decryptingReaderPool = sync.Pool{
	New: func() interface{} {
		return &DecryptingReader{
			chunkBuf: make([]byte, ChunkCipherSize),
			ptBuf:    make([]byte, 0, ChunkPayloadSize),
			aadBuf:   make([]byte, 8+HeaderNonceSize),
		}
	},
}

// CopyBufPool is a shared pool of 32KB buffers for io.CopyBuffer.
var CopyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 32*1024)
		return &b
	},
}

// Reset prepares a DecryptingReader for reuse via sync.Pool.
func (dr *DecryptingReader) Reset(r io.Reader, gcm cipher.AEAD, headerNonce []byte, startChunk uint64) {
	dr.r = r
	dr.gcm = gcm
	dr.headerNonce = headerNonce
	dr.chunkNum = startChunk
	dr.buf = nil
	dr.offset = 0
	dr.done = false
	// Ensure aadBuf is correct size
	needed := 8 + len(headerNonce)
	if cap(dr.aadBuf) >= needed {
		dr.aadBuf = dr.aadBuf[:needed]
	} else {
		dr.aadBuf = make([]byte, needed)
	}
}

// Release returns the DecryptingReader to the pool for reuse.
// After calling Release, do not use the reader.
func (dr *DecryptingReader) Release() {
	dr.r = nil
	dr.gcm = nil
	dr.headerNonce = nil
	dr.buf = nil
	decryptingReaderPool.Put(dr)
}

// NewDecryptingReader creates a reader that decrypts data in chunks.
// The returned reader should be released via Release() when done.
func NewDecryptingReader(r io.Reader, contentKey []byte, headerNonce []byte) (*DecryptingReader, error) {
	gcm, err := NewGCM(contentKey)
	if err != nil {
		return nil, err
	}

	dr := decryptingReaderPool.Get().(*DecryptingReader)
	dr.Reset(r, gcm, headerNonce, 0)
	return dr, nil
}

// NewDecryptingReaderFromChunk creates a reader starting at a specific chunk number.
// The returned reader should be released via Release() when done.
func NewDecryptingReaderFromChunk(r io.Reader, contentKey []byte, headerNonce []byte, startChunk uint64) (*DecryptingReader, error) {
	gcm, err := NewGCM(contentKey)
	if err != nil {
		return nil, err
	}

	dr := decryptingReaderPool.Get().(*DecryptingReader)
	dr.Reset(r, gcm, headerNonce, startChunk)
	return dr, nil
}

func (dr *DecryptingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if dr.offset < len(dr.buf) {
		n := copy(p, dr.buf[dr.offset:])
		dr.offset += n
		return n, nil
	}

	if dr.done {
		return 0, io.EOF
	}

	// Read next encrypted chunk
	n, err := io.ReadFull(dr.r, dr.chunkBuf)
	if n == 0 {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			dr.done = true
			return 0, io.EOF
		}
		return 0, err
	}
	if n < ChunkNonceSize+ChunkTagSize {
		dr.done = true
		return 0, fmt.Errorf("%w: chunk %d is truncated (got %d bytes)", ErrCorruptContent, dr.chunkNum, n)
	}
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, err
	}

	chunkData := dr.chunkBuf[:n]

	nonce := chunkData[:ChunkNonceSize]
	ciphertext := chunkData[ChunkNonceSize:]
	fillAAD(dr.aadBuf, dr.chunkNum, dr.headerNonce)
	plaintext, decErr := dr.gcm.Open(dr.ptBuf[:0], nonce, ciphertext, dr.aadBuf)
	if decErr != nil {
		return 0, fmt.Errorf("%w: decrypt chunk %d: %w", ErrCorruptContent, dr.chunkNum, decErr)
	}

	dr.chunkNum++
	dr.buf = plaintext
	dr.offset = 0

	if err == io.EOF || err == io.ErrUnexpectedEOF {
		dr.done = true
	}

	copied := copy(p, dr.buf[dr.offset:])
	dr.offset += copied
	return copied, nil
}

// PlaintextOffset2CipherOffset converts a plaintext byte offset to a ciphertext offset
// Returns: cipher file offset, chunk index, bytes to skip in first decrypted chunk
func PlaintextOffset2CipherOffset(plaintextOffset int64) (cipherOffset int64, chunkIndex uint64, skipBytes int) {
	chunkIndex = uint64(plaintextOffset / ChunkPayloadSize)
	skipBytes = int(plaintextOffset % ChunkPayloadSize)
	cipherOffset = int64(HeaderSize) + int64(chunkIndex)*int64(ChunkCipherSize)
	return
}

// PlaintextSize2CipherSize converts a plaintext file size to the expected ciphertext file size
func PlaintextSize2CipherSize(plaintextSize int64) int64 {
	if plaintextSize == 0 {
		return int64(HeaderSize)
	}
	fullChunks := plaintextSize / ChunkPayloadSize
	remainder := plaintextSize % ChunkPayloadSize

	size := int64(HeaderSize)
	size += fullChunks * int64(ChunkCipherSize)
	if remainder > 0 {
		size += int64(ChunkNonceSize) + remainder + int64(ChunkTagSize)
	}
	return size
}

// CipherSize2PlaintextSize converts a ciphertext file size to the plaintext file size
func CipherSize2PlaintextSize(cipherSize int64) int64 {
	if cipherSize <= int64(HeaderSize) {
		return 0
	}
	contentSize := cipherSize - int64(HeaderSize)
	fullChunks := contentSize / int64(ChunkCipherSize)
	remainder := contentSize % int64(ChunkCipherSize)

	plaintextSize := fullChunks * ChunkPayloadSize
	if remainder > 0 {
		minimum := int64(ChunkNonceSize + ChunkTagSize)
		if remainder < minimum {
			return plaintextSize
		}
		plaintextSize += remainder - minimum
	}
	return plaintextSize
}
