package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Harvey-AU/blue-banded-bee/src/crawler"
	"github.com/getsentry/sentry-go"
	"github.com/rs/zerolog/log"
)

// JobManager handles job creation and lifecycle management
type JobManager struct {
	db         *sql.DB
	crawler    *crawler.Crawler
	workerPool *WorkerPool
}

// NewJobManager creates a new job manager
func NewJobManager(db *sql.DB, crawler *crawler.Crawler, workerPool *WorkerPool) *JobManager {
	return &JobManager{
		db:         db,
		crawler:    crawler,
		workerPool: workerPool,
	}
}

// CreateJob creates a new job with the given options
func (jm *JobManager) CreateJob(ctx context.Context, options *JobOptions) (*Job, error) {
	span := sentry.StartSpan(ctx, "manager.create_job")
	defer span.Finish()
	
	span.SetTag("domain", options.Domain)

	// Create the job
	job, err := CreateJob(jm.db, options)
	if err != nil {
		span.SetTag("error", "true")
		span.SetData("error.message", err.Error())
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	log.Info().
		Str("job_id", job.ID).
		Str("domain", job.Domain).
		Bool("use_sitemap", options.UseSitemap).
		Bool("find_links", options.FindLinks).
		Int("max_depth", options.MaxDepth).
		Msg("Created new job")

	// Add initial URLs to process
	if len(options.StartURLs) > 0 {
		// Add explicitly provided URLs
		if err := EnqueueURLs(ctx, jm.db, job.ID, options.StartURLs, "manual", "", 0); err != nil {
			span.SetTag("error", "true")
			span.SetData("error.message", err.Error())
			log.Error().Err(err).Msg("Failed to enqueue start URLs")
		}
	} else if options.UseSitemap {
		// Fetch and process sitemap in a separate goroutine
		go jm.processSitemap(context.Background(), job.ID, options.Domain, options.IncludePaths, options.ExcludePaths)
	} else {
		// Add domain root URL as a fallback
		rootURL := fmt.Sprintf("https://%s", options.Domain)
		if err := EnqueueURLs(ctx, jm.db, job.ID, []string{rootURL}, "manual", "", 0); err != nil {
			span.SetTag("error", "true")
			span.SetData("error.message", err.Error())
			log.Error().Err(err).Msg("Failed to enqueue root URL")
		}
	}

	return job, nil
}

// StartJob starts a pending job
func (jm *JobManager) StartJob(ctx context.Context, jobID string) error {
	span := sentry.StartSpan(ctx, "manager.start_job")
	defer span.Finish()
	
	span.SetTag("job_id", jobID)

	// Get the job
	job, err := GetJob(ctx, jm.db, jobID)
	if err != nil {
		span.SetTag("error", "true")
		span.SetData("error.message", err.Error())
		return fmt.Errorf("failed to get job: %w", err)
	}

	// Check if job can be started
	if job.Status != JobStatusPending {
		return fmt.Errorf("job is not in pending state: %s", job.Status)
	}

	// Update job status to running
	job.Status = JobStatusRunning
	job.StartedAt = time.Now()

	_, err = jm.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = ?, started_at = ?
		WHERE id = ?
	`, job.Status, job.StartedAt, job.ID)

	if err != nil {
		span.SetTag("error", "true")
		span.SetData("error.message", err.Error())
		return fmt.Errorf("failed to update job status: %w", err)
	}

	// Add job to worker pool for processing
	jm.workerPool.AddJob(job.ID)

	log.Info().
		Str("job_id", job.ID).
		Str("domain", job.Domain).
		Msg("Started job")

	return nil
}

// CancelJob cancels a running job
func (jm *JobManager) CancelJob(ctx context.Context, jobID string) error {
	span := sentry.StartSpan(ctx, "manager.cancel_job")
	defer span.Finish()
	
	span.SetTag("job_id", jobID)

	// Get the job
	job, err := GetJob(ctx, jm.db, jobID)
	if err != nil {
		span.SetTag("error", "true")
		span.SetData("error.message", err.Error())
		return fmt.Errorf("failed to get job: %w", err)
	}

	// Check if job can be canceled
	if job.Status != JobStatusRunning && job.Status != JobStatusPending && job.Status != JobStatusPaused {
		return fmt.Errorf("job cannot be canceled: %s", job.Status)
	}

	// Update job status to cancelled
	job.Status = JobStatusCancelled
	job.CompletedAt = time.Now()

	_, err = jm.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = ?, completed_at = ?
		WHERE id = ?
	`, job.Status, job.CompletedAt, job.ID)

	if err != nil {
		span.SetTag("error", "true")
		span.SetData("error.message", err.Error())
		return fmt.Errorf("failed to update job status: %w", err)
	}

	// Remove job from worker pool
	jm.workerPool.RemoveJob(job.ID)

	// Cancel pending tasks
	_, err = jm.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?
		WHERE job_id = ? AND status = ?
	`, TaskStatusSkipped, job.ID, TaskStatusPending)

	if err != nil {
		span.SetTag("error", "true")
		span.SetData("error.message", err.Error())
		log.Error().Err(err).Str("job_id", job.ID).Msg("Failed to cancel pending tasks")
	}

	log.Info().
		Str("job_id", job.ID).
		Str("domain", job.Domain).
		Msg("Cancelled job")

	return nil
}

// GetJobStatus gets the current status of a job
func (jm *JobManager) GetJobStatus(ctx context.Context, jobID string) (*Job, error) {
	span := sentry.StartSpan(ctx, "manager.get_job_status")
	defer span.Finish()
	
	span.SetTag("job_id", jobID)

	// Get the job
	job, err := GetJob(ctx, jm.db, jobID)
	if err != nil {
		span.SetTag("error", "true")
		span.SetData("error.message", err.Error())
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	return job, nil
}

// processSitemap fetches and processes a sitemap for a domain
func (jm *JobManager) processSitemap(ctx context.Context, jobID, domain string, includePaths, excludePaths []string) {
	span := sentry.StartSpan(ctx, "manager.process_sitemap")
	defer span.Finish()
	
	span.SetTag("job_id", jobID)
	span.SetTag("domain", domain)

	log.Info().
		Str("job_id", jobID).
		Str("domain", domain).
		Msg("Starting sitemap processing")

	// Create a crawler config that allows skipping already cached URLs
	crawlerConfig := crawler.DefaultConfig()
	crawlerConfig.SkipCachedURLs = false
	sitemapCrawler := crawler.New(crawlerConfig)

	// Discover sitemaps for the domain
	baseURL := fmt.Sprintf("https://%s", domain)
	urls, err := sitemapCrawler.DiscoverSitemaps(ctx, baseURL)
	if err != nil {
		span.SetTag("error", "true")
		span.SetData("error.message", err.Error())
		log.Error().
			Err(err).
			Str("job_id", jobID).
			Str("domain", domain).
			Msg("Failed to discover sitemaps")
		
		// Update job with error
		_, err = jm.db.ExecContext(ctx, `
			UPDATE jobs
			SET error_message = ?
			WHERE id = ?
		`, fmt.Sprintf("Failed to discover sitemaps: %v", err), jobID)
		
		return
	}

	// Filter URLs based on include/exclude patterns
	if len(includePaths) > 0 || len(excludePaths) > 0 {
		urls = sitemapCrawler.FilterURLs(urls, includePaths, excludePaths)
	}

	// Add URLs to the job queue
	if len(urls) > 0 {
		if err := EnqueueURLs(ctx, jm.db, jobID, urls, "sitemap", baseURL, 0); err != nil {
			span.SetTag("error", "true")
			span.SetData("error.message", err.Error())
			log.Error().
				Err(err).
				Str("job_id", jobID).
				Str("domain", domain).
				Msg("Failed to enqueue sitemap URLs")
			return
		}

		log.Info().
			Str("job_id", jobID).
			Str("domain", domain).
			Int("url_count", len(urls)).
			Msg("Added sitemap URLs to job queue")
	} else {
		log.Warn().
			Str("job_id", jobID).
			Str("domain", domain).
			Msg("No URLs found in sitemap")
			
		// Update job with warning
		_, err = jm.db.ExecContext(ctx, `
			UPDATE jobs
			SET error_message = ?
			WHERE id = ?
		`, "No URLs found in sitemap", jobID)
	}

	// Start the job if it's in pending state
	job, err := GetJob(ctx, jm.db, jobID)
	if err == nil && job.Status == JobStatusPending {
		if err := jm.StartJob(ctx, jobID); err != nil {
			log.Error().
				Err(err).
				Str("job_id", jobID).
				Msg("Failed to start job after processing sitemap")
		}
	}
}
