package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"logchipper/internal/allowlist"
	"logchipper/internal/db"
	"logchipper/internal/handler"
	internalsyslog "logchipper/internal/syslog"
)

//go:embed static
var staticFiles embed.FS

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Fatalf("%s: %q is not an integer", key, v)
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("%s: %v", key, err)
		}
		return d
	}
	return fallback
}

func main() {
	dbPath := flag.String("db", envOr("DB_PATH", "./logchipper.db"), "SQLite database path")
	port := flag.String("port", envOr("PORT", "8080"), "HTTP port")
	syslogAddr := flag.String("syslog", envOr("SYSLOG_ADDR", ":514"), "Syslog UDP+TCP listen address")
	authEnabled := flag.Bool("auth", envOr("AUTH", "false") == "true", "Require a login; the first visit asks you to create the account")
	ingestToken := flag.String("ingest-token", envOr("INGEST_TOKEN", ""), "Bearer token required for HTTP log ingest (POST /api/logs, /api/logs/text); empty = ingest follows UI login")
	allowedIPs := flag.String("allowed-ips", envOr("ALLOWED_IPS", ""), "Comma-separated IPs/CIDRs allowed in addition to the access mode (with internet: only these)")
	retentionDays := flag.Int("retention", 0, "Days to retain logs (0 = use RETENTION_DAYS env, default 30)")

	syslogEnabled := flag.Bool("syslog-enable", envOr("SYSLOG_ENABLE", "true") != "false", "Enable syslog listener")
	accessMode := flag.String("access", envOr("ACCESS_MODE", "network"), "Who can connect: local, network or internet")
	maxBody := flag.Int64("max-body", int64(envInt("MAX_BODY_BYTES", 1<<20)), "Max HTTP request body size in bytes")
	maxQueryLimit := flag.Int("max-query-limit", envInt("MAX_QUERY_LIMIT", 1000), "Max events returned by one GET /api/logs")
	maxStreamClients := flag.Int("max-stream-clients", envInt("MAX_STREAM_CLIENTS", 100), "Max concurrent /api/stream connections")
	syslogMaxMsg := flag.Int("syslog-max-message", envInt("SYSLOG_MAX_MESSAGE_BYTES", 8192), "Max syslog message/line size in bytes (longer UDP messages are truncated, longer TCP lines drop the connection)")
	syslogMaxConns := flag.Int("syslog-max-conns", envInt("SYSLOG_MAX_CONNS", 256), "Max concurrent syslog TCP connections")
	readTimeout := flag.Duration("read-timeout", envDuration("HTTP_READ_TIMEOUT", 30*time.Second), "Max time to read an entire HTTP request")
	flag.Parse()

	for name, v := range map[string]int64{
		"max-body": *maxBody, "max-query-limit": int64(*maxQueryLimit), "max-stream-clients": int64(*maxStreamClients),
		"syslog-max-message": int64(*syslogMaxMsg), "syslog-max-conns": int64(*syslogMaxConns), "read-timeout": int64(*readTimeout),
	} {
		if v <= 0 {
			log.Fatalf("%s must be greater than 0", name)
		}
	}

	allow, err := allowlist.New(*accessMode, *allowedIPs)
	if err != nil {
		log.Fatalf("allowed-ips: %v", err)
	}

	if *retentionDays == 0 {
		if v := os.Getenv("RETENTION_DAYS"); v != "" {
			n, err := strconv.Atoi(v)
			if err == nil && n > 0 {
				*retentionDays = n
			}
		}
		if *retentionDays == 0 {
			*retentionDays = 30
		}
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer database.Close()

	broker := handler.NewBroker()
	h := &handler.Handler{
		DB:               database,
		Broker:           broker,
		MaxQueryLimit:    *maxQueryLimit,
		MaxStreamClients: *maxStreamClients,
	}

	// Purge worker
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		retention := time.Duration(*retentionDays) * 24 * time.Hour
		for range ticker.C {
			n, err := database.Purge(retention)
			if err != nil {
				log.Printf("purge error: %v", err)
			} else if n > 0 {
				log.Printf("purged %d events older than %d days", n, *retentionDays)
			}
		}
	}()

	// Syslog listener (UDP + TCP)
	if *syslogEnabled {
		sl := &internalsyslog.Server{
			DB:         database,
			Broker:     broker,
			Allow:      allow,
			MaxMessage: *syslogMaxMsg,
			MaxConns:   *syslogMaxConns,
		}
		if err := sl.ListenUDP(*syslogAddr); err != nil {
			log.Printf("warning: %v", err)
		}
		if err := sl.ListenTCP(*syslogAddr); err != nil {
			log.Printf("warning: %v", err)
		}
	}

	if *syslogEnabled && !allow.Enabled() {
		log.Printf("WARNING: syslog (UDP/TCP %s) has no authentication and ACCESS_MODE=internet with no ALLOWED_IPS: anyone who can reach it can inject logs. Restrict with ACCESS_MODE/ALLOWED_IPS or a firewall/security group, or set SYSLOG_ENABLE=false", *syslogAddr)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.IngestLog(w, r)
		case http.MethodGet:
			h.QueryLogs(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/logs/text", h.IngestSyslog)
	mux.HandleFunc("/api/sources", h.Sources)
	mux.HandleFunc("/api/logs/histogram", h.Histogram)
	mux.HandleFunc("/api/stream", h.Stream)
	mux.HandleFunc("/api/dbsize", h.DBSize)

	// Non-secret settings the UI needs to build the connection strings.
	_, syslogPort, _ := net.SplitHostPort(*syslogAddr)
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"syslog_enabled":       *syslogEnabled,
			"syslog_port":          syslogPort,
			"ingest_token_enabled": *ingestToken != "",
		})
	})

	// Health check endpoint
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	})

	// Auth endpoints
	auth := handler.NewAuth(*authEnabled, database)
	mux.HandleFunc("/api/login", auth.Login)
	mux.HandleFunc("/api/logout", auth.Logout)
	mux.HandleFunc("/api/setup", auth.Setup)
	mux.HandleFunc("/api/auth/status", auth.Status)

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		fileServer.ServeHTTP(w, r)
	}))

	// Wrap mux with auth middleware
	var httpHandler http.Handler = mux
	if *authEnabled {
		httpHandler = auth.Middleware(mux)
	}

	if *ingestToken != "" {
		httpHandler = handler.IngestToken(*ingestToken, *authEnabled, mux, httpHandler)
	}
	httpHandler = handler.LimitBody(*maxBody, httpHandler)
	httpHandler = allow.Middleware(httpHandler)

	addr := fmt.Sprintf(":%s", *port)
	log.Printf("logchipper listening on %s (retention=%d days, db=%s)", addr, *retentionDays, *dbPath)
	if allow.Enabled() {
		log.Printf("access mode=%s allowed-ips=%q", *accessMode, *allowedIPs)
	}
	log.Printf("open %s", startupURL(*port))
	if setupRequired, err := auth.SetupRequired(); err != nil {
		log.Fatalf("auth: %v", err)
	} else if setupRequired {
		log.Printf("auth: no account yet. Open the URL above to create one")
		if !allow.Enabled() {
			log.Printf("WARNING: ACCESS_MODE=internet and no account exists: whoever opens the UI first can create it. Set it up now")
		}
	}
	// No WriteTimeout: it would kill long-lived /api/stream (SSE) connections.
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       *readTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// startupURL returns a browsable URL using the host's outbound IP.
func startupURL(port string) string {
	host := "localhost"
	if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		host = conn.LocalAddr().(*net.UDPAddr).IP.String()
		conn.Close()
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}
