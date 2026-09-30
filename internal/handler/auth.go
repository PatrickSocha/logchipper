package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"logchipper/internal/db"
)

const minPasswordLength = 8

const sessionTTL = 90 * 24 * time.Hour

// Auth guards the UI and API with a single account stored in the users table.
// While the table is empty, the first visitor is asked to create it via /api/setup.
type Auth struct {
	Enabled  bool
	DB       *db.DB
	mu       sync.RWMutex
	sessions map[string]time.Time
}

func NewAuth(enabled bool, database *db.DB) *Auth {
	a := &Auth{
		Enabled:  enabled,
		DB:       database,
		sessions: make(map[string]time.Time),
	}
	go a.sessionCleanup()
	return a
}

// Middleware checks cookie; returns 401 JSON if invalid. Skips auth if disabled.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always allow these paths (auth endpoints and health check)
		switch r.URL.Path {
		case "/api/login", "/api/logout", "/api/setup", "/api/auth/status", "/healthz":
			next.ServeHTTP(w, r)
			return
		}

		// If auth is disabled, allow all
		if !a.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		// Allow static files and root (HTML, JS, CSS, etc.)
		// They will detect auth requirement on load via /api/sources probe
		if !isAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// For API endpoints, check session cookie
		cookie, err := r.Cookie("logchipper_session")
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		a.mu.RLock()
		expiry, exists := a.sessions[cookie.Value]
		a.mu.RUnlock()

		if !exists || time.Now().After(expiry) {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isAPIPath checks if a URL path is an API endpoint (requires auth)
func isAPIPath(path string) bool {
	return len(path) >= 4 && path[:4] == "/api"
}

// Login handles POST /api/login with {username, password} JSON
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		bodyError(w, err, `{"error":"bad request"}`)
		return
	}

	hash, err := a.DB.PasswordHash(creds.Username)
	if err != nil {
		log.Printf("login: %v", err)
		http.Error(w, `{"error":"server error"}`, http.StatusInternalServerError)
		return
	}
	if !checkPassword(hash, creds.Password) {
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	a.startSession(w)
}

// Setup handles POST /api/setup with {username, password} JSON. It creates the
// one account and signs it in, and only works while no account exists.
func (a *Auth) Setup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !a.Enabled {
		http.Error(w, `{"error":"auth is disabled"}`, http.StatusNotFound)
		return
	}

	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		bodyError(w, err, `{"error":"bad request"}`)
		return
	}
	creds.Username = strings.TrimSpace(creds.Username)
	if creds.Username == "" {
		http.Error(w, `{"error":"username is required"}`, http.StatusBadRequest)
		return
	}
	if len(creds.Password) < minPasswordLength {
		http.Error(w, `{"error":"password must be at least 8 characters"}`, http.StatusBadRequest)
		return
	}

	hash, err := hashPassword(creds.Password)
	if err != nil {
		http.Error(w, `{"error":"server error"}`, http.StatusInternalServerError)
		return
	}
	created, err := a.DB.CreateFirstUser(creds.Username, hash)
	if err != nil {
		log.Printf("setup: %v", err)
		http.Error(w, `{"error":"server error"}`, http.StatusInternalServerError)
		return
	}
	if !created {
		http.Error(w, `{"error":"an account already exists"}`, http.StatusConflict)
		return
	}
	log.Printf("auth: account %q created", creds.Username)

	a.startSession(w)
}

// Status handles GET /api/auth/status so the UI knows whether to show the
// login form or the first-run setup form.
func (a *Auth) Status(w http.ResponseWriter, r *http.Request) {
	setupRequired, err := a.SetupRequired()
	if err != nil {
		http.Error(w, `{"error":"server error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{
		"auth_enabled":   a.Enabled,
		"setup_required": setupRequired,
	})
}

// SetupRequired reports whether auth is on but no account has been created yet.
func (a *Auth) SetupRequired() (bool, error) {
	if !a.Enabled {
		return false, nil
	}
	n, err := a.DB.UserCount()
	return n == 0, err
}

// startSession issues a session cookie and writes {"status":"ok"}.
func (a *Auth) startSession(w http.ResponseWriter) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		http.Error(w, `{"error":"server error"}`, http.StatusInternalServerError)
		return
	}
	tokenStr := hex.EncodeToString(token)

	// Store session
	a.mu.Lock()
	a.sessions[tokenStr] = time.Now().Add(sessionTTL)
	a.mu.Unlock()

	// Set cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "logchipper_session",
		Value:    tokenStr,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Logout handles POST /api/logout
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("logchipper_session")
	if err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}

	// Clear cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "logchipper_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// sessionCleanup evicts expired sessions every minute
func (a *Auth) sessionCleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		a.mu.Lock()
		now := time.Now()
		for token, expiry := range a.sessions {
			if now.After(expiry) {
				delete(a.sessions, token)
			}
		}
		a.mu.Unlock()
	}
}

// secureEqual compares in constant time (hashing first so length doesn't leak).
func secureEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func isIngest(r *http.Request) bool {
	return r.Method == http.MethodPost && (r.URL.Path == "/api/logs" || r.URL.Path == "/api/logs/text")
}

// IngestToken lets log shippers authenticate HTTP ingest with
// "Authorization: Bearer <token>" instead of a UI session. A valid token goes
// straight to ingest. Otherwise the request falls through to fallback (the
// session-cookie check); if the UI has no login (uiAuth false), non-token
// ingest is rejected outright so the token actually protects the endpoint.
func IngestToken(token string, uiAuth bool, ingest, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isIngest(r) {
			fallback.ServeHTTP(w, r)
			return
		}
		if got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && secureEqual(got, token) {
			ingest.ServeHTTP(w, r)
			return
		}
		if !uiAuth {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}
