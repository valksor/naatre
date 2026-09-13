// Package processhost is a minimal net/http example for the process lifecycle
// contract. It is not the Naatre HTTP protocol adapter or a process supervisor.
package processhost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/valksor/naatre/runtime"
)

// Admission maps already-authenticated host state to bounded process weights.
// Implementations must not trust caller-supplied tenant or principal headers.
type Admission func(*http.Request) runtime.AdmissionRequest

// Business is invoked only after process admission succeeds.
type Business func(context.Context, runtime.AdmittedWork, http.ResponseWriter, *http.Request) error

// NewHandler exposes /health/live, /health/ready, and /work for a reference
// process host. The caller owns authentication, request decoding, and weights.
func NewHandler(process *runtime.ProcessController, admission Admission, business Business) (http.Handler, error) {
	if process == nil {
		return nil, errors.New("process controller is nil")
	}
	if admission == nil {
		return nil, errors.New("process admission mapper is nil")
	}
	if business == nil {
		return nil, errors.New("process business callback is nil")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeHealth(writer, process.Health(), false)
	})
	mux.HandleFunc("GET /health/ready", func(writer http.ResponseWriter, _ *http.Request) {
		writeHealth(writer, process.Health(), true)
	})
	mux.HandleFunc("POST /work", func(writer http.ResponseWriter, request *http.Request) {
		tracked := &responseWriter{ResponseWriter: writer}
		err := process.Run(request.Context(), admission(request), func(ctx context.Context, work runtime.AdmittedWork) error {
			return business(ctx, work, tracked, request)
		})
		if err == nil || tracked.wroteHeader {
			return
		}
		var rejected *runtime.AdmissionError
		if errors.As(err, &rejected) {
			status := http.StatusServiceUnavailable
			if rejected.Code == runtime.CodeRateLimited {
				status = http.StatusTooManyRequests
			}
			seconds := max(int64(1), int64((rejected.RetryAfter+time.Second-1)/time.Second))
			tracked.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			writeProblem(tracked, status, rejected.Code)
			return
		}
		writeProblem(tracked, http.StatusInternalServerError, runtime.CodeInternal)
	})
	return mux, nil
}

type responseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (writer *responseWriter) WriteHeader(status int) {
	writer.wroteHeader = true
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *responseWriter) Write(payload []byte) (int, error) {
	writer.wroteHeader = true
	return writer.ResponseWriter.Write(payload)
}

func writeHealth(writer http.ResponseWriter, health runtime.ProcessHealth, readiness bool) {
	status := http.StatusOK
	if (readiness && !health.Ready) || (!readiness && !health.Live) {
		status = http.StatusServiceUnavailable
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(health)
}

func writeProblem(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}{Status: status, Code: code})
}
