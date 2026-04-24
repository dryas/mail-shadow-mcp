// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// fileserver.go:
// Optional lightweight HTTP server that serves attachment files via
// time-limited, single-use download tokens. Useful as a fallback when the
// AI agent cannot transfer files through its normal communication channels.
package fileserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// entry is a single registered download token.
type entry struct {
	filePath string
	expiry   time.Time
}

// Server manages download tokens and serves files over HTTP.
type Server struct {
	mu     sync.Mutex
	tokens map[string]entry
	port   int
	ttl    time.Duration
	host   string
}

// New creates a Server and starts listening on the given port.
// ttlMin is the lifetime of each token in minutes (default 15).
// host is the externally reachable hostname/IP (default "localhost").
func New(port int, ttlMin int, host string) (*Server, error) {
	if ttlMin <= 0 {
		ttlMin = 15
	}
	if host == "" {
		host = "localhost"
	}

	s := &Server{
		tokens: make(map[string]entry),
		port:   port,
		ttl:    time.Duration(ttlMin) * time.Minute,
		host:   host,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/dl/", s.handleDownload)

	srv := &http.Server{
		Addr:        fmt.Sprintf(":%d", port),
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
	}

	go func() {
		slog.Info("fileserver listening", "port", port, "ttl_min", ttlMin)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("fileserver error", "err", err)
		}
	}()

	// Background goroutine to clean up expired tokens.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.purgeExpired()
		}
	}()

	return s, nil
}

// CreateLink registers a file for download and returns a URL valid for the TTL.
func (s *Server) CreateLink(filePath string) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("fileserver: generate token: %w", err)
	}

	s.mu.Lock()
	s.tokens[token] = entry{
		filePath: filePath,
		expiry:   time.Now().Add(s.ttl),
	}
	s.mu.Unlock()

	filename := filepath.Base(filePath)
	url := fmt.Sprintf("http://%s:%d/dl/%s/%s", s.host, s.port, token, filename)
	return url, nil
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	// URL format: /dl/<token>/<filename>
	// We only need the token part; the filename in the URL is for browser UX.
	path := r.URL.Path[len("/dl/"):] // strip "/dl/"
	slash := len(path)
	for i, c := range path {
		if c == '/' {
			slash = i
			break
		}
	}
	token := path[:slash]
	if token == "" {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	e, ok := s.tokens[token]
	if ok {
		delete(s.tokens, token) // single-use
	}
	s.mu.Unlock()

	if !ok || time.Now().After(e.expiry) {
		http.Error(w, "link expired or not found", http.StatusGone)
		return
	}

	filename := filepath.Base(e.filePath)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	http.ServeFile(w, r, e.filePath)
}

func (s *Server) purgeExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, e := range s.tokens {
		if now.After(e.expiry) {
			delete(s.tokens, token)
		}
	}
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
