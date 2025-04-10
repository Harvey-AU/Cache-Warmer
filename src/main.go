package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Harvey-AU/blue-banded-bee/src/crawler"
	"github.com/Harvey-AU/blue-banded-bee/src/db"
	"github.com/getsentry/sentry-go"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/time/rate"
)

// @title Blue Banded Bee API
// @version 1.0
// @description A web crawler service that warms caches and tracks response times
// @host blue-banded-bee.fly.dev
// @BasePath /

// Config holds the application configuration loaded from environment variables
type Config struct {
	Port        string // HTTP port to listen on
	Env         string // Environment (development/production)
	LogLevel    string // Logging level
	DatabaseURL string // Turso database URL
	AuthToken   string // Database authentication token
	SentryDSN   string // Sentry DSN for error tracking
}

func setupLogging(config *Config) {
	// Set up pretty console logging for development
	if config.Env == "development" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})
	}

	// Set log level
	level, err := zerolog.ParseLevel(config.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)
}

func loadConfig() (*Config, error) {
	// Load .env file if it exists
	godotenv.Load()

	config := &Config{
		Port:        getEnvWithDefault("PORT", "8080"),
		Env:         getEnvWithDefault("APP_ENV", "development"),
		LogLevel:    getEnvWithDefault("LOG_LEVEL", "info"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		AuthToken:   os.Getenv("DATABASE_AUTH_TOKEN"),
		SentryDSN:   os.Getenv("SENTRY_DSN"),
	}

	// Validate configuration
	if err := validateConfig(config); err != nil {
		return nil, err
	}

	return config, nil
}

// getEnvWithDefault retrieves an environment variable or returns a default value if not set
func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// addSentryContext adds request context information to Sentry for error tracking
func addSentryContext(r *http.Request, name string) {
	if hub := sentry.GetHubFromContext(r.Context()); hub != nil {
		hub.Scope().SetTag("endpoint", name)
		hub.Scope().SetTag("method", r.Method)
		hub.Scope().SetTag("user_agent", r.UserAgent())
	}
}

// statusRecorder wraps an http.ResponseWriter to capture the status code
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
	size       int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	size, err := r.ResponseWriter.Write(b)
	r.size += size
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return size, err
}

// wrapWithSentryTransaction wraps an HTTP handler with Sentry transaction monitoring
func wrapWithSentryTransaction(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		transaction := sentry.StartTransaction(r.Context(), name)
		defer transaction.Finish()

		rw := &statusRecorder{ResponseWriter: w}
		next(rw, r.WithContext(transaction.Context()))

		transaction.SetTag("http.method", r.Method)
		transaction.SetTag("http.url", r.URL.String())
		transaction.SetTag("http.status_code", fmt.Sprintf("%d", rw.statusCode))
		transaction.SetData("response_size", rw.size)
	}
}

// rateLimiter implements a per-IP rate limiting mechanism
type rateLimiter struct {
	visitors map[string]*rate.Limiter
	mu       sync.Mutex
}

// newRateLimiter creates a new rate limiter instance
func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		visitors: make(map[string]*rate.Limiter),
	}
}

// getLimiter returns a rate limiter for the given IP address
func (rl *rateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.visitors[ip]
	if !exists {
		limiter = rate.NewLimiter(rate.Every(1*time.Second), 5) // 5 requests per second
		rl.visitors[ip] = limiter
	}

	return limiter
}

// getClientIP extracts the real client IP address from a request
// It checks X-Forwarded-For and X-Real-IP headers before falling back to RemoteAddr
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first (common in proxies)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		// The leftmost IP is the original client
		return strings.TrimSpace(ips[0])
	}
	
	// Then X-Real-IP (used by Nginx and others)
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return xrip
	}
	
	// Fallback to RemoteAddr, but strip the port
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// If we couldn't split, just use the whole thing
		return r.RemoteAddr
	}
	return ip
}

// middleware implements rate limiting for HTTP handlers
func (rl *rateLimiter) middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get the real client IP
		ip := getClientIP(r)
		
		// Get or create a rate limiter for this IP
		limiter := rl.getLimiter(ip)
		
		// Check if the request exceeds the rate limit
		if !limiter.Allow() {
			log.Info().
				Str("ip", ip).
				Str("endpoint", r.URL.Path).
				Msg("Rate limit exceeded")
				
			// Track in Sentry
			if hub := sentry.GetHubFromContext(r.Context()); hub != nil {
				hub.Scope().SetTag("rate_limited", "true")
				hub.Scope().SetTag("client_ip", ip)
				hub.CaptureMessage("Rate limit exceeded")
			}
			
			// Return 429 Too Many Requests
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		
		next(w, r)
	}
}

// sanitizeURL removes sensitive information from URLs before logging
func sanitizeURL(urlStr string) string {
	if urlStr == "" {
		return ""
	}
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return "invalid-url"
	}
	// Remove query parameters and userinfo
	parsed.RawQuery = ""
	parsed.User = nil
	return parsed.String()
}

// generateResetToken creates a secure token for development endpoints
func generateResetToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

var resetToken = generateResetToken()

// Metrics tracks various performance and usage metrics for the application
type Metrics struct {
	ResponseTimes []time.Duration // List of response times
	CacheHits     int             // Number of cache hits
	CacheMisses   int             // Number of cache misses
	ErrorCount    int             // Number of errors encountered
	RequestCount  int             // Total number of requests
	mu           sync.Mutex
}

// recordMetrics records various metrics about a request
func (m *Metrics) recordMetrics(duration time.Duration, cacheStatus string, hasError bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ResponseTimes = append(m.ResponseTimes, duration)
	m.RequestCount++

	if hasError {
		m.ErrorCount++
	}

	if cacheStatus == "HIT" {
		m.CacheHits++
	} else {
		m.CacheMisses++
	}
}

var metrics = &Metrics{}

func main() {
	// Load configuration
	config, err := loadConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load configuration")
	}

	// Setup logging
	setupLogging(config)

	// Initialize Sentry
	if config.SentryDSN != "" {
		err = sentry.Init(sentry.ClientOptions{
			Dsn:              config.SentryDSN,
			Environment:      config.Env,
			TracesSampleRate: 1.0,
			EnableTracing:    true,
			Debug:            config.Env == "development",
		})
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to initialize Sentry")
		}
		defer sentry.Flush(2 * time.Second)
	} else {
		log.Warn().Msg("Sentry not initialized: SENTRY_DSN not provided")
	}

	// Initialize rate limiter
	limiter := newRateLimiter()

	// Health check handler
	// @Summary Health check endpoint
	// @Description Returns the deployment time of the service
	// @Tags Health
	// @Produce plain
	// @Success 200 {string} string "OK - Deployed at: {timestamp}"
	// @Router /health [get]
	http.HandleFunc("/health", limiter.middleware(wrapWithSentryTransaction("health", func(w http.ResponseWriter, r *http.Request) {
		log.Debug().Str("endpoint", "/health").Msg("Health check requested")
		w.Header().Set("Content-Type", "text/plain")
		const healthFormat = "OK - Deployed at: %s"
		response := fmt.Sprintf(healthFormat, time.Now().Format(time.RFC3339))
		fmt.Fprintln(w, response)
	})))

	// Test crawl handler
	// @Summary Test crawl endpoint
	// @Description Crawls a URL and returns the result
	// @Tags Crawler
	// @Param url query string false "URL to crawl (defaults to teamharvey.co)"
	// @Produce json
	// @Success 200 {object} crawler.CrawlResult
	// @Failure 400 {string} string "Invalid URL"
	// @Failure 500 {string} string "Internal server error"
	// @Router /test-crawl [get]
	http.HandleFunc("/test-crawl", limiter.middleware(wrapWithSentryTransaction("test-crawl", func(w http.ResponseWriter, r *http.Request) {
		span := sentry.StartSpan(r.Context(), "crawl.process")
		defer span.Finish()

		url := r.URL.Query().Get("url")
		if url == "" {
			url = "https://www.teamharvey.co"
		}
		
		// Add this line to get skip_cached parameter
		skipCached := r.URL.Query().Get("skip_cached") == "true"
		
		// Modify crawler initialization to include config
		crawlerConfig := crawler.DefaultConfig()
		crawlerConfig.SkipCachedURLs = skipCached
		
		crawler := crawler.New(crawlerConfig)
		
		sanitizedURL := sanitizeURL(url)
		span.SetTag("crawl.url", sanitizedURL)

		// Database connection span
		dbSpan := sentry.StartSpan(r.Context(), "db.connect")
		dbConfig := &db.Config{
			URL:       config.DatabaseURL,
			AuthToken: config.AuthToken,
		}
		database, err := db.GetInstance(dbConfig)
		if err != nil {
			dbSpan.SetTag("error", "true")
			dbSpan.Finish()
			sentry.CaptureException(err)
			log.Error().Err(err).Msg("Failed to connect to database")
			http.Error(w, "Database connection failed", http.StatusInternalServerError)
			return
		}
		dbSpan.Finish()

		// Crawl span
		crawlSpan := sentry.StartSpan(r.Context(), "crawl.execute")
		result, err := crawler.WarmURL(r.Context(), url)
		if err != nil {
			crawlSpan.SetTag("error", "true")
			crawlSpan.SetTag("error.type", "url_parse_error")
			crawlSpan.SetData("error.message", err.Error())
			result.Error = err.Error()
			crawlSpan.Finish()
			sentry.CaptureException(err)
			log.Error().Err(err).Msg("Crawl failed")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		crawlSpan.SetTag("status_code", fmt.Sprintf("%d", result.StatusCode))
		crawlSpan.SetTag("cache_status", result.CacheStatus)
		crawlSpan.SetData("response_time_ms", result.ResponseTime)
		crawlSpan.Finish()

		// Store result span
		storeSpan := sentry.StartSpan(r.Context(), "db.store_result")
		crawlResult := &db.CrawlResult{
			URL:          result.URL,
			ResponseTime: result.ResponseTime,
			StatusCode:   result.StatusCode,
			Error:        result.Error,
			CacheStatus:  result.CacheStatus,
		}

		if err := database.StoreCrawlResult(r.Context(), crawlResult); err != nil {
			storeSpan.SetTag("error", "true")
			storeSpan.Finish()
			log.Error().Err(err).Msg("Failed to store crawl result")
			http.Error(w, "Failed to store result", http.StatusInternalServerError)
			return
		}
		storeSpan.Finish()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})))

	// Recent crawls endpoint
	// @Summary Recent crawls endpoint
	// @Description Returns the 10 most recent crawl results
	// @Tags Crawler
	// @Produce json
	// @Success 200 {array} db.CrawlResult
	// @Failure 500 {string} string "Internal server error"
	// @Router /recent-crawls [get]
	http.HandleFunc("/recent-crawls", limiter.middleware(wrapWithSentryTransaction("recent-crawls", func(w http.ResponseWriter, r *http.Request) {
		span := sentry.StartSpan(r.Context(), "db.get_recent")
		defer span.Finish()

		dbConfig := &db.Config{
			URL:       config.DatabaseURL,
			AuthToken: config.AuthToken,
		}

		database, err := db.GetInstance(dbConfig)
		if err != nil {
			span.SetTag("error", "true")
			sentry.CaptureException(err)
			log.Error().Err(err).Msg("Failed to connect to database")
			http.Error(w, "Database connection failed", http.StatusInternalServerError)
			return
		}

		results, err := database.GetRecentResults(r.Context(), 10)
		if err != nil {
			span.SetTag("error", "true")
			sentry.CaptureException(err)
			log.Error().Err(err).Msg("Failed to get recent results")
			http.Error(w, "Failed to get results", http.StatusInternalServerError)
			return
		}

		span.SetData("result_count", len(results))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})))

	// Reset database endpoint (development only)
	if config.Env == "development" {
		devToken := generateResetToken()
		log.Info().Msg("Development reset token generated - check logs for value")
		http.HandleFunc("/reset-db", limiter.middleware(wrapWithSentryTransaction("reset-db", func(w http.ResponseWriter, r *http.Request) {
			// Only allow POST method
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			// Verify token
			authHeader := r.Header.Get("Authorization")
			if authHeader != fmt.Sprintf("Bearer %s", devToken) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			dbConfig := &db.Config{
				URL:       config.DatabaseURL,
				AuthToken: config.AuthToken,
			}

			database, err := db.GetInstance(dbConfig)
			if err != nil {
				log.Error().Err(err).Msg("Failed to connect to database")
				http.Error(w, "Database connection failed", http.StatusInternalServerError)
				return
			}

			if err := database.ResetSchema(); err != nil {
				sentry.CaptureException(err)
				log.Error().Err(err).Msg("Failed to reset database schema")
				http.Error(w, "Failed to reset database", http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"status": "Database schema reset successfully",
			})
		})))
	}

	// Create a new HTTP server
	server := &http.Server{
		Addr: ":" + config.Port,
		Handler: nil, // Uses the default ServeMux
		
	}

	// Channel to listen for termination signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Channel to signal when the server has shut down
	done := make(chan struct{})

	go func() {
		<-stop // Wait for a termination signal
		log.Info().Msg("Shutting down server...")

		// Create a context with a timeout for the shutdown process
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Attempt to gracefully shut down the server
		if err := server.Shutdown(ctx); err != nil {
			log.Error().Err(err).Msg("Server forced to shutdown")
		}

		close(done)
	}()

	// Start the server
	log.Info().Msgf("Starting server on port %s", config.Port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("Server error")
	}

	<-done // Wait for the shutdown process to complete
	log.Info().Msg("Server stopped")
}

// Add this function to validate configuration
func validateConfig(config *Config) error {
	var errors []string

	// Check required values
	if config.DatabaseURL == "" {
		errors = append(errors, "DATABASE_URL is required")
	}

	if config.AuthToken == "" {
		errors = append(errors, "DATABASE_AUTH_TOKEN is required")
	}

	// Validate environment
	if config.Env != "development" && config.Env != "production" && config.Env != "staging" {
		errors = append(errors, fmt.Sprintf("APP_ENV must be one of [development, production, staging], got %s", config.Env))
	}

	// Validate port
	if _, err := strconv.Atoi(config.Port); err != nil {
		errors = append(errors, fmt.Sprintf("PORT must be a valid number, got %s", config.Port))
	}

	// Check log level
	_, err := zerolog.ParseLevel(config.LogLevel)
	if err != nil {
		errors = append(errors, fmt.Sprintf("LOG_LEVEL %s is invalid, using default: info", config.LogLevel))
	}

	// Warn about missing Sentry DSN
	if config.SentryDSN == "" {
		log.Warn().Msg("SENTRY_DSN is not set, error tracking will be disabled")
	}

	if len(errors) > 0 {
		return fmt.Errorf("configuration validation failed: %s", strings.Join(errors, "; "))
	}

	return nil
}
