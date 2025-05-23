package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Harvey-AU/blue-banded-bee/internal/crawler"
	"github.com/Harvey-AU/blue-banded-bee/internal/db"
	"github.com/Harvey-AU/blue-banded-bee/internal/jobs"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/time/rate"
)

// Config holds the application configuration loaded from environment variables
type Config struct {
	Port      string // HTTP port to listen on
	Env       string // Environment (development/production)
	SentryDSN string // Sentry DSN for error tracking
	LogLevel  string // Log level (debug, info, warn, error)
}

func main() {
	// Load .env file if it exists
	godotenv.Load()

	// Load configuration
	config := &Config{
		Port:      getEnvWithDefault("PORT", "8080"),
		Env:       getEnvWithDefault("APP_ENV", "development"),
		SentryDSN: os.Getenv("SENTRY_DSN"),
		LogLevel:  getEnvWithDefault("LOG_LEVEL", "info"),
	}

	setupLogging(config)

	// Connect to PostgreSQL
	pgDB, err := db.InitFromEnv()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to PostgreSQL database")
	}
	defer pgDB.Close()

	log.Info().Msg("Connected to PostgreSQL database")

	// Initialise crawler
	crawlerConfig := crawler.DefaultConfig()
	cr := crawler.New(crawlerConfig)

	// Create database queue for operations
	dbQueue := db.NewDbQueue(pgDB.GetDB())
	
	// Create a worker pool for task processing
	var jobWorkers int = 5
	workerPool := jobs.NewWorkerPool(pgDB.GetDB(), dbQueue, cr, jobWorkers, pgDB.GetConfig())
	
	// Create job manager first
	jobsManager := jobs.NewJobManager(pgDB.GetDB(), dbQueue, cr, workerPool)
	
	// Set the job manager in the worker pool for duplicate checking
	workerPool.SetJobManager(jobsManager)
	
	// Start the worker pool now that it's fully configured
	workerPool.Start(context.Background())
	defer workerPool.Stop()

	// Start a goroutine to monitor job completion
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		// Use for-range instead of for-select for better readability
		for range ticker.C {
			// Check for jobs that should be marked complete
			rows, err := pgDB.GetDB().Query(`
				UPDATE jobs 
				SET status = 'completed', completed_at = NOW()
				WHERE (completed_tasks + failed_tasks) >= total_tasks 
				  AND status = 'running'
				RETURNING id
			`)
			if err != nil {
				log.Error().Err(err).Msg("Failed to update completed jobs")
				continue
			}

			// Log completed jobs
			for rows.Next() {
				var jobID string
				if err := rows.Scan(&jobID); err == nil {
					log.Info().Str("job_id", jobID).Msg("Job marked as completed")
				}
			}
			rows.Close()
		}
	}()

	// Create a rate limiter
	limiter := newRateLimiter()

	// HTTP endpoints
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "OK",
			"time":   time.Now().Format(time.RFC3339),
		})
	})

	http.HandleFunc("/pg-health", func(w http.ResponseWriter, r *http.Request) {
		if err := pgDB.GetDB().Ping(); err != nil {
			log.Error().Err(err).Msg("PostgreSQL health check failed")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"status": "ERROR",
				"error":  err.Error(),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "OK",
			"time":   time.Now().Format(time.RFC3339),
		})
	})

	// Add a reset-db endpoint
	// TODO: Remove this after core testing, or add auth
	http.HandleFunc("/reset-db", func(w http.ResponseWriter, r *http.Request) {
		log.Warn().Msg("Database reset requested")

		if err := pgDB.ResetSchema(); err != nil {
			log.Error().Err(err).Msg("Failed to reset database schema")
			http.Error(w, "Failed to reset database", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "Database schema reset successfully",
		})
	})

	http.HandleFunc("/site", func(w http.ResponseWriter, r *http.Request) {
		domain := r.URL.Query().Get("domain")
		if domain == "" {
			http.Error(w, "Domain parameter is required", http.StatusBadRequest)
			return
		}

		// Limit number of pages to be crawled
		maxPages := 0
		if maxStr := r.URL.Query().Get("max"); maxStr != "" {
			parsed, err := strconv.Atoi(maxStr)
			if err != nil || parsed < 1 {
				http.Error(w, "Invalid max parameter", http.StatusBadRequest)
				return
			}
			maxPages = parsed
		}

		// Extract hyperlinks (including PDFs/docs)
		findLinks := false
		if flStr := r.URL.Query().Get("find_links"); flStr != "" {
			v, err := strconv.ParseBool(flStr)
			if err != nil {
				http.Error(w, "Invalid find_links parameter", http.StatusBadRequest)
				return
			}
			findLinks = v
		}

		// Override sitemap default flag
		useSitemap := true
		if sitemapStr := r.URL.Query().Get("sitemap"); sitemapStr != "" {
			v, err := strconv.ParseBool(sitemapStr)
			if err != nil {
				http.Error(w, "Invalid sitemap parameter", http.StatusBadRequest)
				return
			}
			useSitemap = v
		}

		// Override concurrency default flag
		jobConcurrency := 5
		if concurrencyStr := r.URL.Query().Get("concurrency"); concurrencyStr != "" {
			v, err := strconv.Atoi(concurrencyStr)
			if err != nil {
				http.Error(w, "Invalid concurrency parameter", http.StatusBadRequest)
				return
			}
			jobConcurrency = v
		}

		opts := &jobs.JobOptions{
			Domain:      domain,
			UseSitemap:  useSitemap,
			Concurrency: jobConcurrency,
			FindLinks:   findLinks,
			MaxPages:    maxPages,
		}
		job, err := jobsManager.CreateJob(r.Context(), opts)
		if err != nil {
			log.Error().Err(err).Str("domain", domain).Msg("Failed to create job")
			http.Error(w, "Failed to create job", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":      "OK",
			"job_id":      job.ID,
			"domain":      job.Domain,
			"use_sitemap": strconv.FormatBool(useSitemap),
			"concurrency": strconv.Itoa(jobConcurrency),
			"find_links":  strconv.FormatBool(findLinks),
			"max_pages":   strconv.Itoa(maxPages),
		})
	})

	http.HandleFunc("/job-status", func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Query().Get("job_id")
		if jobID == "" {
			http.Error(w, "job_id parameter required", http.StatusBadRequest)
			return
		}

		var total, completed, failed int
		var status string
		err := pgDB.GetDB().QueryRowContext(r.Context(), `
			SELECT total_tasks, completed_tasks, failed_tasks, status 
			FROM jobs WHERE id = $1
		`, jobID).Scan(&total, &completed, &failed, &status)

		if err != nil {
			http.Error(w, "Job not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"job_id":    jobID,
			"status":    status,
			"total":     total,
			"completed": completed,
			"failed":    failed,
			"progress":  float64(completed+failed) / float64(total) * 100,
		})
	})

	// Create a new HTTP server
	server := &http.Server{
		Addr: ":" + config.Port,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := getClientIP(r)
			if !limiter.getLimiter(ip).Allow() {
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			http.DefaultServeMux.ServeHTTP(w, r)
		}),
	}

	// Channel to listen for termination signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Channel to signal when the server has shut down
	done := make(chan struct{})

	go func() {
		<-stop
		log.Info().Msg("Shutting down server...")

		// Create shutdown context with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		defer cancel()

		// Stop accepting new requests
		if err := server.Shutdown(ctx); err != nil {
			log.Error().Err(err).Msg("Server forced to shutdown")
		}

		close(done)
	}()

	// Start the server
	log.Info().Str("port", config.Port).Msg("Starting server")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("Server error")
	}

	<-done // Wait for the shutdown process to complete
	log.Info().Msg("Server stopped")
}

// getEnvWithDefault retrieves an environment variable or returns a default value if not set
func getEnvWithDefault(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

// setupLogging configures the logging system
func setupLogging(config *Config) {
	// Configure log level
	level, err := zerolog.ParseLevel(config.LogLevel)
	if err != nil {
		level = zerolog.WarnLevel
	}
	zerolog.SetGlobalLevel(level)

	// Use console writer in development
	if config.Env == "development" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})
	} else {
		// In production, use a more verbose JSON format that works well with Fly.io logs
		log.Logger = zerolog.New(os.Stdout).
			With().
			Timestamp().
			Str("service", "blue-banded-bee").
			Logger()
	}
}

// RateLimiter represents a rate limiting system based on client IP addresses
type RateLimiter struct {
	limits   map[string]*IPRateLimiter
	mu       sync.Mutex
	rate     rate.Limit
	capacity int
}

// IPRateLimiter wraps a token bucket rate limiter specific to an IP address
type IPRateLimiter struct {
	limiter *rate.Limiter
}

// newRateLimiter creates a new rate limiter with default settings
func newRateLimiter() *RateLimiter {
	return &RateLimiter{
		limits:   make(map[string]*IPRateLimiter),
		rate:     rate.Limit(5), // 5 requests per second
		capacity: 5,             // 5 burst capacity
	}
}

// getLimiter returns the rate limiter for a specific IP address
func (rl *RateLimiter) getLimiter(ip string) *IPRateLimiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.limits[ip]
	if !exists {
		limiter = &IPRateLimiter{
			limiter: rate.NewLimiter(rl.rate, rl.capacity),
		}
		rl.limits[ip] = limiter
	}

	return limiter
}

// Allow checks if a request from this IP should be allowed
func (ipl *IPRateLimiter) Allow() bool {
	return ipl.limiter.Allow()
}

// getClientIP extracts the client's IP address from a request
func getClientIP(r *http.Request) string {
	// Check for X-Forwarded-For header first (for clients behind proxies)
	ip := r.Header.Get("X-Forwarded-For")
	if ip != "" {
		// X-Forwarded-For might contain multiple IPs, take the first one
		ips := strings.Split(ip, ",")
		ip = strings.TrimSpace(ips[0])
		return ip
	}

	// If no X-Forwarded-For, use RemoteAddr
	ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	return ip
}
