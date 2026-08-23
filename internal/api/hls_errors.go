package api

import (
	"context"
	"errors"
	"net/http"
)

// errHLSCapacity is returned when the server has reached its configured
// active/pending stream budget. It is kept as a domain error so every HLS
// entrypoint exposes the same status and machine-readable code.
var errHLSCapacity = errors.New("hls stream capacity reached")

// hlsStartHTTPStatus is the HTTP adapter for the HLS domain's startup
// lifecycle errors. Keeping this pure mapping outside the large handler file
// makes the error contract easy to test and prevents transport concerns from
// leaking into the stream state machine.
func hlsStartHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errHLSCapacity):
		return http.StatusTooManyRequests
	case errors.Is(err, context.Canceled):
		return http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func hlsStartErrorCode(err error) string {
	switch {
	case errors.Is(err, errHLSCapacity):
		return "hls_capacity_exceeded"
	case errors.Is(err, context.Canceled):
		return "hls_start_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "hls_start_timeout"
	default:
		return "hls_start_failed"
	}
}
