package main

import (
	"context"
	"log/slog"
	"net/http"

	pkgerr "github.com/pkg/errors"
	"golang.org/x/crypto/bcrypt"
)

type contextKey string

const UserContextKey contextKey = "user"

var allowedUsers = map[string]string{
	"frodo":   "$2a$10$B6O/n6teuCzpuh66jrUAdeaJ3WvXcxRkzpN0x7H.di9G9e/NGb9Me",
	"samwise": "$2a$10$EWZpvYhUJtJcEMmm/IBOsOGIcpxUnGIVMRiDlN/nxl1RRwWGkJtty",
	// frodo: "ofTheNineFingers"
	// samwise: "theStrong"
	"saruman": "invalidFormat",
}

func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			httpError(r.Context(), w, http.StatusUnauthorized, pkgerr.New("Unauthorized"))
			return
		}
		stored, exists := allowedUsers[username]
		if !exists {
			httpError(r.Context(), w, http.StatusUnauthorized, pkgerr.New("Unauthorized"))
			return
		}
		ok, err := s.validatePassword(password, stored)
		if err != nil {
			s.logger.Error("error validating password", slog.String("user", username), slog.Any("error", err))
			httpError(r.Context(), w, http.StatusInternalServerError, pkgerr.New("Internal Server Error"))
			return
		}
		if !ok {
			httpError(r.Context(), w, http.StatusUnauthorized, pkgerr.New("Unauthorized"))
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), UserContextKey, username))
		
		value := r.Context().Value(logContextKey)
		if logCtx, ok := value.(*LogContext); ok {
			logCtx.Username = username
		}

		next.ServeHTTP(w, r)
	})
}

func (s *server) validatePassword(password, stored string) (bool, error) {
	_, span := tracer.Start(context.Background(), "auth.validate_password")
	defer span.End()
	err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return false, nil
	}
	if err != nil {
		return false, pkgerr.WithStack(err)
	}
	return true, nil
}
