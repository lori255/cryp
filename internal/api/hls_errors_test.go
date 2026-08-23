package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestHLSStartHTTPStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "capacity", err: errHLSCapacity, want: http.StatusTooManyRequests},
		{name: "cancelled", err: context.Canceled, want: http.StatusConflict},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{name: "wrapped cancel", err: errors.Join(errors.New("startup"), context.Canceled), want: http.StatusConflict},
		{name: "other", err: errors.New("startup failed"), want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hlsStartHTTPStatus(tt.err); got != tt.want {
				t.Fatalf("hlsStartHTTPStatus(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestHLSStartErrorCode(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{err: errHLSCapacity, want: "hls_capacity_exceeded"},
		{err: context.Canceled, want: "hls_start_cancelled"},
		{err: context.DeadlineExceeded, want: "hls_start_timeout"},
		{err: errors.New("ffmpeg failed"), want: "hls_start_failed"},
	}
	for _, tt := range tests {
		if got := hlsStartErrorCode(tt.err); got != tt.want {
			t.Fatalf("hlsStartErrorCode(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
