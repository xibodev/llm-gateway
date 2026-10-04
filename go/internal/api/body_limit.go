package api

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// defaultMaxRequestBodyBytes bounds a request body when
// LLMGW_MAX_REQUEST_BODY_BYTES is unset. Handlers hold a body in memory to
// decode or re-encode it, so without a bound one request can exhaust the
// process, and a multipart upload can fill the disk it spills to. 64 MiB
// leaves room for long contexts, inline images and transcription audio.
const defaultMaxRequestBodyBytes = 64 << 20

// lowestMaxRequestBodyBytes is the smallest limit the variable can set. A
// lower one would refuse ordinary prompts and console saves, which reads as a
// mistake rather than a policy.
const lowestMaxRequestBodyBytes = 1 << 20

const requestBodyTooLarge = "request body too large"

// maxRequestBodyBytes is the largest request body the gateway reads.
func maxRequestBodyBytes() int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("LLMGW_MAX_REQUEST_BODY_BYTES")), 10, 64)
	switch {
	case err != nil || value <= 0:
		return defaultMaxRequestBodyBytes
	case value < lowestMaxRequestBodyBytes:
		return lowestMaxRequestBodyBytes
	}
	return value
}

// limitRequestBodies bounds every request body by maxRequestBodyBytes. A
// declared length over the limit is refused before the handler runs; a body
// without one is cut off by http.MaxBytesReader where it crosses the limit,
// and the handler answers that through writeBodyError. Handlers with a
// tighter limit of their own keep it.
func limitRequestBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			limit := maxRequestBodyBytes()
			if r.ContentLength > limit {
				writeError(w, http.StatusRequestEntityTooLarge, requestBodyTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// writeBodyError answers a request whose body could not be read or decoded:
// 413 when the body crossed its size limit, otherwise status and message. It
// returns the status it wrote, for the caller's usage record.
func writeBodyError(w http.ResponseWriter, err error, status int, message string) int {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status, message = http.StatusRequestEntityTooLarge, requestBodyTooLarge
	}
	writeError(w, status, message)
	return status
}
