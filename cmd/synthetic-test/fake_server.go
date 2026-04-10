package main

import (
	"fmt"
	"net"
	"net/http"
)

// FakeZoomServer serves predetermined bytes for download URLs the bridge will
// request, simulating Zoom's recording file delivery. It enforces that requests
// include an access_token query parameter, proving the bridge is authenticating
// the way Zoom requires (per-event download_token, not the OAuth bearer token).
type FakeZoomServer struct {
	server   *http.Server
	listener net.Listener
	files    map[string][]byte // path → bytes to serve
	hits     map[string]int    // path → number of times requested (for assertions)
}

// StartFakeZoomServer binds to a random local port and serves the given files.
// Caller must call Close() when done.
func StartFakeZoomServer(files map[string][]byte) (*FakeZoomServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	f := &FakeZoomServer{
		listener: listener,
		files:    files,
		hits:     make(map[string]int),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)

	f.server = &http.Server{Handler: mux}
	go func() {
		_ = f.server.Serve(listener)
	}()

	return f, nil
}

// URL returns the base URL clients can use to reach the fake server.
func (f *FakeZoomServer) URL() string {
	return "http://" + f.listener.Addr().String()
}

// Close shuts down the server.
func (f *FakeZoomServer) Close() error {
	return f.server.Close()
}

// Hits returns the number of times a given path was requested.
func (f *FakeZoomServer) Hits(path string) int {
	return f.hits[path]
}

func (f *FakeZoomServer) handle(w http.ResponseWriter, r *http.Request) {
	// Verify the bridge is authenticating with the access_token query param.
	// This is the auth model Zoom requires for webhook downloads — we assert
	// it here because forgetting it would be a regression of the bug we just
	// fixed in main.go.
	if r.URL.Query().Get("access_token") == "" {
		http.Error(w, "missing access_token query parameter", http.StatusUnauthorized)
		return
	}

	bytes, ok := f.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}

	f.hits[r.URL.Path]++
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bytes)))
	_, _ = w.Write(bytes)
}
