package main

// The HTTP side of Delta: the JSON API used by the web interface (and by
// scripts), the password login, live updates via server-sent events, and the
// embedded static files of the web interface.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const sessionCookieName = "delta_session"

type Server struct {
	manager     *Manager
	sessionsMu  sync.Mutex
	sessions    map[string]time.Time // session token -> expiry
	loginMu     sync.Mutex           // serializes login attempts (brute-force protection)
	staticFiles http.Handler

	// Checking a password hash takes ~0.2 s on purpose. Scripts that use HTTP
	// basic auth send the password with every request, so correct passwords
	// are remembered (by a fingerprint, never in plain text) for this hash.
	basicAuthMu    sync.Mutex
	basicAuthValid map[[32]byte]bool
}

func newServer(manager *Manager) *Server {
	webFiles, _ := fs.Sub(embeddedWebFiles, "web")
	return &Server{
		manager: manager, sessions: map[string]time.Time{}, staticFiles: http.FileServerFS(webFiles),
		basicAuthValid: map[[32]byte]bool{},
	}
}

// routes registers all URLs. Everything below /api except login/logout
// requires a valid session or the password via HTTP basic auth.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/setup", s.handleSetupStatus) // public: tells the UI whether to show the setup page
	mux.HandleFunc("POST /api/setup", s.handleSetup)      // public, but only works while no password is set
	protected := func(pattern string, handler http.HandlerFunc) { mux.Handle(pattern, s.requireLogin(handler)) }

	protected("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
	})
	protected("GET /api/forwarders", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.manager.List())
	})
	protected("POST /api/forwarders", s.handleCreate)
	protected("GET /api/forwarders/{id}", s.handleGet)
	protected("PUT /api/forwarders/{id}", s.handleUpdate)
	protected("DELETE /api/forwarders/{id}", s.handleDelete)
	protected("POST /api/forwarders/{id}/{action}", s.handleAction)
	protected("GET /api/forwarders/{id}/history", s.handleHistory)
	protected("GET /api/forwarders/{id}/inspect", s.handleInspect)
	protected("POST /api/bulk", s.handleBulk)
	protected("GET /api/stream", s.handleLiveStream)
	protected("GET /api/events", s.handleEvents)
	protected("GET /api/interfaces", s.handleInterfaces)
	protected("GET /api/export", s.handleExport)
	protected("POST /api/import", s.handleImport)
	protected("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.manager.Settings())
	})
	protected("PUT /api/settings", s.handleUpdateSettings)
	protected("POST /api/settings/test-webhook", s.handleTestWebhook)
	protected("GET /api/captures", s.handleListCaptures)
	protected("POST /api/captures", s.handleUploadCapture)
	protected("GET /api/captures/{name}", s.handleDownloadCapture)
	protected("DELETE /api/captures/{name}", s.handleDeleteCapture)
	protected("POST /api/captures/{name}/replay", s.handleReplay)
	protected("GET /api/captures/{name}/records", s.handleCaptureRecords)
	mux.Handle("/", s.staticFiles) // the web interface itself is public; it shows the login form

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Basic browser hardening: no framing, no MIME sniffing, no external resources.
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		header.Set("Cache-Control", "no-cache")
		mux.ServeHTTP(w, r)
	})
}

// ---- authentication ----

// checkBasicAuthPassword verifies a password sent via HTTP basic auth, using
// the cache described at Server.basicAuthValid.
func (s *Server) checkBasicAuthPassword(password string) bool {
	storedHash := s.manager.PasswordHash()
	if storedHash == "" {
		return false
	}
	// The fingerprint includes the stored hash, so a password change invalidates old entries.
	fingerprint := sha256.Sum256([]byte(storedHash + "\x00" + password))
	s.basicAuthMu.Lock()
	known := s.basicAuthValid[fingerprint]
	s.basicAuthMu.Unlock()
	if known {
		return true
	}
	if !verifyPassword(password, storedHash) {
		return false
	}
	s.basicAuthMu.Lock()
	if len(s.basicAuthValid) > 100 {
		clear(s.basicAuthValid) // keep the cache small
	}
	s.basicAuthValid[fingerprint] = true
	s.basicAuthMu.Unlock()
	return true
}

// requireLogin lets a request through if it has a valid session cookie or
// HTTP basic auth with the password (any user name), e.g. for curl.
// Until a password is set, everything except the setup page is closed.
func (s *Server) requireLogin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(sessionCookieName); err == nil {
			s.sessionsMu.Lock()
			expiresAt, exists := s.sessions[cookie.Value]
			s.sessionsMu.Unlock()
			if exists && time.Now().Before(expiresAt) {
				next(w, r)
				return
			}
		}
		if _, password, ok := r.BasicAuth(); ok && s.checkBasicAuthPassword(password) {
			next(w, r)
			return
		}
		if s.manager.NeedsPasswordSetup() {
			writeError(w, http.StatusUnauthorized, errors.New("no password set yet: open the web interface to set one"))
			return
		}
		writeError(w, http.StatusUnauthorized, errors.New("login required"))
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Only one attempt at a time, and a wrong password costs one second:
	// this limits guessing to one password per second.
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.manager.NeedsPasswordSetup() {
		writeError(w, http.StatusConflict, errors.New("no password set yet"))
		return
	}
	if !verifyPassword(request.Password, s.manager.PasswordHash()) {
		time.Sleep(time.Second)
		s.manager.events.Add("warn", "", "", "failed login from "+r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, errors.New("wrong password"))
		return
	}
	s.startSession(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if !s.manager.NeedsPasswordSetup() {
		writeJSON(w, http.StatusOK, map[string]any{"setup_required": false})
		return
	}
	// Only while setup is pending: the values the first-start page starts with.
	writeJSON(w, http.StatusOK, map[string]any{"setup_required": true, "defaults": s.manager.SetupDefaults()})
}

// handleSetup saves the first password and settings and logs the browser in.
// It only works while no password exists (enforced by CompleteSetup).
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var request InitialSetup
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	restartNeeded, err := s.manager.CompleteSetup(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	slog.Info("first-start setup completed", "from", r.RemoteAddr)
	s.startSession(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_needed": restartNeeded})
}

// startSession creates a new session and sends its cookie to the browser.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	token := randomHex(32)
	expiresAt := time.Now().Add(time.Duration(s.manager.SessionHours()) * time.Hour)
	s.sessionsMu.Lock()
	for oldToken, oldExpiry := range s.sessions { // clean up expired sessions
		if time.Now().After(oldExpiry) {
			delete(s.sessions, oldToken)
		}
	}
	s.sessions[token] = expiresAt
	s.sessionsMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", Expires: expiresAt,
		HttpOnly: true,                    // not readable by JavaScript
		SameSite: http.SameSiteStrictMode, // not sent by other sites (CSRF protection)
		Secure:   r.TLS != nil,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessionsMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- forwarders ----

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var fc ForwarderConfig
	if err := readJSON(r, &fc); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status, err := s.manager.Create(fc, r.URL.Query().Get("start") == "1")
	if err != nil && status.ID == "" { // invalid config: nothing was created
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// If only the start failed, the forwarder exists and its status shows the error.
	writeJSON(w, http.StatusCreated, status)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	status, found := s.manager.Get(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, errNotFound)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var fc ForwarderConfig
	if err := readJSON(r, &fc); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status, err := s.manager.Update(r.PathValue("id"), fc)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, err)
	case err != nil && status.ID == "":
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, status)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAction handles POST /api/forwarders/{id}/{action}.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch action := r.PathValue("action"); action {
	case "start", "stop", "restart":
		err = s.manager.SetRunning(action, id)
	case "kick": // disconnect one peer
		var request struct {
			Endpoint string `json:"endpoint"` // "in" or "out:<index>"
			PeerID   uint64 `json:"peer"`
		}
		if err = readJSON(r, &request); err == nil && !s.manager.DisconnectPeer(id, request.Endpoint, request.PeerID) {
			err = errors.New("client not found")
		}
	case "capture-start":
		var request struct {
			MaxSeconds   int `json:"max_seconds"`
			MaxMegabytes int `json:"max_mb"`
		}
		if err = readJSON(r, &request); err == nil {
			_, err = s.manager.StartCapture(id, request.MaxSeconds, request.MaxMegabytes)
		}
	case "capture-stop":
		err = s.manager.StopCapture(id)
	case "replay-stop":
		err = s.manager.StopReplay(id)
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown action %q", action))
		return
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNotFound) {
			code = http.StatusNotFound
		}
		writeError(w, code, err)
		return
	}
	s.manager.BroadcastForwarders()
	status, _ := s.manager.Get(id)
	writeJSON(w, http.StatusOK, status)
}

func (m *Manager) StopReplay(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[id]
	if inst == nil {
		return errNotFound
	}
	if inst.replay == nil {
		return errors.New("no replay running")
	}
	inst.replay.stop()
	return nil
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	history, found := s.manager.History(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, errNotFound)
		return
	}
	writeJSON(w, http.StatusOK, history)
}

// handleBulk applies one action to several forwarders (multi-select in the UI).
func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
		Value  string   `json:"value"` // tag name
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var err error
	switch request.Action {
	case "start", "stop", "restart":
		err = s.manager.SetRunning(request.Action, request.IDs...)
	case "delete":
		err = s.manager.Delete(request.IDs...)
	case "tag", "untag":
		err = s.manager.UpdateTags(request.Action, request.Value, request.IDs...)
	default:
		err = fmt.Errorf("unknown action %q", request.Action)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", `attachment; filename="delta-forwarders-`+time.Now().Format("20060102")+`.json"`)
	writeJSON(w, http.StatusOK, map[string]any{"forwarders": s.manager.Export(r.URL.Query()["id"])})
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Forwarders []ForwarderConfig `json:"forwarders"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	count, err := s.manager.Import(request.Forwarders)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": count})
}

// ---- live data (server-sent events) ----

// startEventStream sends the headers of a server-sent event stream.
func startEventStream(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if ok {
		header := w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("X-Accel-Buffering", "no") // tell nginx not to buffer the stream
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
	}
	return flusher, ok
}

// handleLiveStream keeps a connection open and pushes the forwarder list
// (every second and after every change) and new events to the browser.
func (s *Server) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	updates := s.manager.liveUpdates.Subscribe()
	defer s.manager.liveUpdates.Unsubscribe(updates)
	flusher, ok := startEventStream(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	initialList, _ := json.Marshal(s.manager.List())
	fmt.Fprintf(w, "event: forwarders\ndata: %s\n\n", initialList)
	flusher.Flush()

	keepAlive := time.NewTicker(20 * time.Second) // keeps proxies from closing an idle connection
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case message := <-updates:
			if _, err := w.Write(message); err != nil {
				return
			}
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}

// inspectedMessage is one row of the live inspector or the capture viewer.
type inspectedMessage struct {
	TimeMillis float64 `json:"t"` // live: unix ms; capture: ms since the capture started
	Source     string  `json:"s"`
	Length     int     `json:"n"` // full length; Data may be shortened
	Data       []byte  `json:"d"` // the first previewBytes bytes, base64 in JSON
}

const (
	defaultPreviewBytes = 1024
	maxPreviewBytes     = 16384
	// The browser receives at most this much message data per second, no
	// matter how much flows through the forwarder (a big bit stream can be
	// many MB/s, far more than a browser should render).
	maxInspectBytesPerSecond = 256 << 10
)

// previewBytesParam reads ?bytes= (how much of each message to send).
func previewBytesParam(r *http.Request) int {
	previewBytes, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
	if previewBytes <= 0 {
		return defaultPreviewBytes
	}
	return min(previewBytes, maxPreviewBytes)
}

func previewOf(data []byte, previewBytes int) []byte {
	if len(data) > previewBytes {
		return data[:previewBytes]
	}
	return data
}

// handleInspect streams the data passing through a running forwarder to the
// inspector, in batches every 200 ms. Two limits keep the browser responsive:
// at most `rate` messages per second (default 100) and at most
// maxInspectBytesPerSecond of data; each message is cut to `bytes` bytes
// (default 1024). Everything over the limits is counted as skipped. It reads
// from a tap that drops data rather than wait, so inspecting never slows
// forwarding.
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	fw := s.manager.RunningForwarder(r.PathValue("id"))
	if fw == nil {
		writeError(w, http.StatusConflict, errors.New("forwarder is not running"))
		return
	}
	maxPerSecond, _ := strconv.Atoi(r.URL.Query().Get("rate"))
	if maxPerSecond <= 0 || maxPerSecond > 2000 {
		maxPerSecond = 100
	}
	previewBytes := previewBytesParam(r)
	tapped := fw.taps.subscribe(4096)
	defer fw.taps.unsubscribe(tapped)
	flusher, ok := startEventStream(w)
	if !ok {
		return
	}

	const batchesPerSecond = 5
	batchTicker := time.NewTicker(time.Second / batchesPerSecond)
	defer batchTicker.Stop()
	// Two token buckets, refilled every batch interval: one counts messages, one bytes.
	tokens, refillPerBatch := float64(maxPerSecond), float64(maxPerSecond)/batchesPerSecond
	byteTokens, byteRefillPerBatch := maxInspectBytesPerSecond, maxInspectBytesPerSecond/batchesPerSecond
	var batch []inspectedMessage
	skipped, skippedAtLastSend := 0, 0
	quietBatches := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-fw.ctx.Done(): // the forwarder was stopped
			io.WriteString(w, "event: end\ndata: {}\n\n")
			flusher.Flush()
			return
		case message := <-tapped:
			data := previewOf(message.Data, previewBytes)
			if tokens < 1 || byteTokens < len(data) {
				skipped++
				continue
			}
			tokens--
			byteTokens -= len(data)
			batch = append(batch, inspectedMessage{
				TimeMillis: float64(message.Time) / float64(time.Millisecond),
				Source:     message.Source, Length: len(message.Data), Data: data,
			})
		case <-batchTicker.C:
			tokens = min(tokens+refillPerBatch, float64(maxPerSecond))
			byteTokens = min(byteTokens+byteRefillPerBatch, maxInspectBytesPerSecond)
			if len(batch) == 0 && skipped == skippedAtLastSend {
				// Nothing new: only send an empty batch every 15 s as keep-alive.
				if quietBatches++; quietBatches < 15*batchesPerSecond {
					continue
				}
			}
			quietBatches = 0
			payload, _ := json.Marshal(map[string]any{"rows": batch, "skipped": skipped})
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
			batch, skippedAtLastSend = batch[:0], skipped
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > eventLogCapacity {
		limit = 500
	}
	writeJSON(w, http.StatusOK, s.manager.events.NewestFirst(r.URL.Query().Get("forwarder"), limit))
}

// ---- settings ----

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var update SettingsUpdate
	if err := readJSON(r, &update); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.manager.UpdateSettings(update); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.manager.Settings())
}

func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	var request struct {
		URL string `json:"url"`
	}
	if err := readJSON(r, &request); err != nil || request.URL == "" {
		writeError(w, http.StatusBadRequest, errors.New("webhook url required"))
		return
	}
	text := "[Delta] Test notification — webhook works."
	payload := map[string]any{"text": text, "content": text, "status": "TEST", "time": time.Now().UTC()}
	if err := s.manager.postWebhook(request.URL, payload); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- captures ----

func (s *Server) handleListCaptures(w http.ResponseWriter, r *http.Request) {
	files, err := s.manager.CaptureFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

// handleUploadCapture takes the raw file as request body; ?name= is the original file name.
func (s *Server) handleUploadCapture(w http.ResponseWriter, r *http.Request) {
	name, err := s.manager.ImportCapture(r.URL.Query().Get("name"), r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": name})
}

// handleDownloadCapture sends the .dcap file, or with ?format=raw only the
// recorded data without timestamps (for NMEA this is a plain log file).
func (s *Server) handleDownloadCapture(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := s.manager.capturePath(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("capture not found"))
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.URL.Query().Get("format") != "raw" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		io.Copy(w, file)
		return
	}
	baseName := name[:len(name)-len(".dcap")]
	w.Header().Set("Content-Disposition", `attachment; filename="`+baseName+`.bin"`)
	readDcap(file, func(_ int64, _ string, data []byte) error {
		_, err := w.Write(data)
		return err
	})
}

// handleCaptureRecords returns one page of a capture for the capture viewer.
// Parameters: from (byte position from the previous page's "next", 0 = start),
// limit (records per page), bytes (preview size), filter (text, or /regex/).
func (s *Server) handleCaptureRecords(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	from, _ := strconv.ParseInt(query.Get("from"), 10, 64)
	limit, _ := strconv.Atoi(query.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	matches, err := parseRecordFilter(query.Get("filter"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	page, err := s.manager.ReadCapturePage(r.PathValue("name"), from, limit, previewBytesParam(r), matches)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleDeleteCapture(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.DeleteCapture(r.PathValue("name")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	var options ReplayOptions
	if err := readJSON(r, &options); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.manager.StartReplay(r.PathValue("name"), options); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.manager.BroadcastForwarders()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- helpers ----

// readJSON decodes the request body (at most 4 MB). An empty body is allowed.
func readJSON(r *http.Request, v any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := decoder.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid request: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(v)
}

// writeError sends {"error": "..."}; the web interface shows this text to the user.
func writeError(w http.ResponseWriter, statusCode int, err error) {
	writeJSON(w, statusCode, map[string]string{"error": err.Error()})
}

// localAddress is one IP address of this machine, offered as a bind address
// for listening endpoints in the editor.
type localAddress struct {
	Address   string `json:"address"`
	Interface string `json:"interface"`
	Loopback  bool   `json:"loopback"`
}

// handleInterfaces lists the IP addresses of the network interfaces that are
// up: IPv4 first, then IPv6. IPv6 link-local addresses (fe80::) are left out,
// because binding to them also needs the interface name as zone.
func (s *Server) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	interfaces, err := net.Interfaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var ipv4, ipv6 []localAddress
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ipNetwork, ok := address.(*net.IPNet)
			if !ok || ipNetwork.IP.IsLinkLocalUnicast() {
				continue
			}
			entry := localAddress{Address: ipNetwork.IP.String(), Interface: networkInterface.Name, Loopback: ipNetwork.IP.IsLoopback()}
			if ipNetwork.IP.To4() != nil {
				ipv4 = append(ipv4, entry)
			} else {
				ipv6 = append(ipv6, entry)
			}
		}
	}
	writeJSON(w, http.StatusOK, append(append([]localAddress{}, ipv4...), ipv6...)) // never null
}
