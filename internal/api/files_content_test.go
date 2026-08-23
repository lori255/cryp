package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"cryp/internal/crypto"

	"github.com/gin-gonic/gin"
)

type encryptedRangeFixture struct {
	file      *os.File
	header    *crypto.FileHeader
	totalSize int64
}

func newEncryptedRangeFixture(t *testing.T, plaintext []byte) *encryptedRangeFixture {
	t.Helper()
	masterKey := bytes.Repeat([]byte{0x11}, crypto.MasterKeySize)
	contentKey := bytes.Repeat([]byte{0x22}, crypto.MasterKeySize)
	file, err := os.CreateTemp(t.TempDir(), "range-*.cryp")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	header, err := crypto.WriteFileHeader(file, masterKey, contentKey)
	if err != nil {
		file.Close()
		t.Fatalf("WriteFileHeader: %v", err)
	}
	writer, err := crypto.NewEncryptingWriter(file, header.ContentKey, header.Nonce)
	if err != nil {
		file.Close()
		t.Fatalf("NewEncryptingWriter: %v", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		file.Close()
		t.Fatalf("EncryptingWriter.Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		file.Close()
		t.Fatalf("EncryptingWriter.Close: %v", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		t.Fatalf("Seek: %v", err)
	}
	readHeader, err := crypto.ReadFileHeader(file, masterKey)
	if err != nil {
		file.Close()
		t.Fatalf("ReadFileHeader: %v", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		t.Fatalf("Stat: %v", err)
	}
	return &encryptedRangeFixture{
		file:      file,
		header:    readHeader,
		totalSize: crypto.CipherSize2PlaintextSize(info.Size()),
	}
}

func (f *encryptedRangeFixture) Close() {
	if f != nil && f.file != nil {
		_ = f.file.Close()
	}
}

func runRangeRequest(f *encryptedRangeFixture, rangeHeader string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/content", nil)
	c.Request = request
	(&Server{}).handleRangeRequest(c, f.file, f.header, f.totalSize, "video/mp4", rangeHeader)
	return recorder
}

func TestHandleRangeRequestServesRFC7233Shapes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plaintext := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	tests := []struct {
		name      string
		rangeHead string
		wantBody  string
		wantRange string
	}{
		{name: "explicit", rangeHead: "bytes=3-7", wantBody: "34567", wantRange: "bytes 3-7/36"},
		{name: "suffix", rangeHead: "bytes=-4", wantBody: "wxyz", wantRange: "bytes 32-35/36"},
		{name: "open ended", rangeHead: "bytes=30-", wantBody: "uvwxyz", wantRange: "bytes 30-35/36"},
		{name: "first satisfiable multi range", rangeHead: "bytes=1-2,10-12", wantBody: "12", wantRange: "bytes 1-2/36"},
		{name: "probe at logical eof", rangeHead: "bytes=36-", wantBody: "z", wantRange: "bytes 35-35/36"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newEncryptedRangeFixture(t, plaintext)
			defer fixture.Close()
			response := runRangeRequest(fixture, tt.rangeHead)
			if response.Code != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206; body=%s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Range"); got != tt.wantRange {
				t.Fatalf("Content-Range = %q, want %q", got, tt.wantRange)
			}
			if got := response.Body.String(); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
			if got := response.Header().Get("Accept-Ranges"); got != "bytes" {
				t.Fatalf("Accept-Ranges = %q, want bytes", got)
			}
		})
	}
}

func TestHandleRangeRequestRejectsUnsatisfiableRanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := newEncryptedRangeFixture(t, []byte("content"))
	defer fixture.Close()

	for _, rangeHeader := range []string{"bytes=99-100", "bytes=-0", "bytes=6-2", "not-a-range"} {
		t.Run(rangeHeader, func(t *testing.T) {
			response := runRangeRequest(fixture, rangeHeader)
			if response.Code != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("status = %d, want 416", response.Code)
			}
			want := "bytes */7"
			if got := response.Header().Get("Content-Range"); got != want {
				t.Fatalf("Content-Range = %q, want %q", got, want)
			}
		})
	}

	zero := newEncryptedRangeFixture(t, nil)
	defer zero.Close()
	response := runRangeRequest(zero, "bytes=0-0")
	if response.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("empty-file status = %d, want 416", response.Code)
	}
}
