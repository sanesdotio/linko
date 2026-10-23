package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/prometheus/client_golang/prometheus"
)

type LogContext struct {
	Username string
	Error error
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode int
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

const logContextKey = "log_context"

func (s *spyResponseWriter) Write(b []byte) (int, error) {
	if s.statusCode == 0 {
		s.statusCode = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytesWritten += n
	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (s *spyReadCloser) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	s.bytesRead += n
	return n, err
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}



func requestLogger(logger *slog.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			spyReader := &spyReadCloser{ReadCloser: r.Body}
			spyWriter := &spyResponseWriter{ResponseWriter: w}
			r.Body = spyReader

			logCtx := &LogContext{}
			r = r.WithContext(context.WithValue(r.Context(), logContextKey, logCtx))

			next.ServeHTTP(spyWriter, r)

			attributes := []any{
				"request_id", r.Header.Get("X-Request-ID"),
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", redactIP(r.RemoteAddr),
				"duration", time.Since(start),
				"request_body_bytes", spyReader.bytesRead,
				"response_status", spyWriter.statusCode,
				"response_body_bytes", spyWriter.bytesWritten,
			}

			if logCtx.Username != "" {
				attributes = append(attributes, "user", logCtx.Username)
			}

			if logCtx.Error != nil {
				attributes = append(attributes, "error", logCtx.Error)
			}

			logger.Info("Served request", attributes...)
		})
	}
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	http.Error(w, err.Error(), status)
}

func httpRequestId(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestId := r.Header.Get("X-Request-ID"); requestId != "" {
			w.Header().Set("X-Request-ID", requestId)
		}
		next.ServeHTTP(w, r)
	})
}

func redactIP(address string) string {
	// Return the IP address with the last octet redacted for IPv4 and return non-IPV4 addresses as-is.
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return address
	}
	lastDot := -1
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == '.' {
			lastDot = i
			break
		}
	}
	if lastDot != -1 {
		host = host[:lastDot+1] + "x"
	}
	return host
}

// httpRequestsTotal counts requests by method, path and status.
var httpRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	},
	[]string{"method", "path", "status"},
)

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(rec, r)

		path := r.URL.Path
		method := r.Method
		status := strconv.Itoa(rec.status)

		slog.String("http_request", fmt.Sprintf("%s %s %s", method, path, status))
		httpRequestsTotal.
			WithLabelValues(method, path, status).
			Inc()
	})
}