package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DbQueue is a PostgreSQL implementation of a job queue
type DbQueue struct {
	db *sql.DB
}

// NewDbQueue creates a PostgreSQL job queue
func NewDbQueue(db *sql.DB) *DbQueue {
	return &DbQueue{
		db: db,
	}
}

// Execute runs a database operation in a transaction
func (q *DbQueue) Execute(ctx context.Context, fn func(*sql.Tx) error) error {
	// Begin transaction
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Run the operation
	if err := fn(tx); err != nil {
		return err
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// Task represents a task in the queue
type Task struct {
	ID          string
	JobID       string
	PageID      int
	Path        string
	Status      string
	Depth       int
	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
	RetryCount  int
	Error       string
	SourceType  string
	SourceURL   string

	// Result data
	StatusCode   int
	ResponseTime int64
	CacheStatus  string
	ContentType  string
}

// GetNextPendingTask gets a pending task using row-level locking
func (q *DbQueue) GetNextPendingTask(ctx context.Context, jobID string) (*Task, error) {
	var task Task

	err := q.Execute(ctx, func(tx *sql.Tx) error {
		// Query for a pending task with FOR UPDATE SKIP LOCKED
		// This allows concurrent workers to each get different tasks
		query := `
			SELECT id, job_id, page_id, path, depth, created_at, retry_count, source_type, source_url 
			FROM tasks 
			WHERE status = 'pending'
		`

		// Add job filter if specified
		args := []interface{}{}
		if jobID != "" {
			query += " AND job_id = $1"
			args = append(args, jobID)
		}

		// Add ordering and locking
		query += `
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		`

		// Execute the query
		var row *sql.Row
		if len(args) > 0 {
			row = tx.QueryRowContext(ctx, query, args...)
		} else {
			row = tx.QueryRowContext(ctx, query)
		}

		err := row.Scan(
			&task.ID, &task.JobID, &task.PageID, &task.Path,
			&task.Depth, &task.CreatedAt, &task.RetryCount, &task.SourceType, &task.SourceURL,
		)

		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		if err != nil {
			return fmt.Errorf("failed to query task: %w", err)
		}

		// Update the task status
		now := time.Now()
		_, err = tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'running', started_at = $1
			WHERE id = $2
		`, now, task.ID)

		if err != nil {
			return fmt.Errorf("failed to update task status: %w", err)
		}

		task.Status = "running"
		task.StartedAt = now

		return nil
	})

	if err == sql.ErrNoRows {
		return nil, nil // No tasks available
	}
	if err != nil {
		return nil, err
	}

	return &task, nil
}

// EnqueueURLs adds multiple URLs as tasks for a job
func (q *DbQueue) EnqueueURLs(ctx context.Context, jobID string, pageIDs []int, paths []string, sourceType string, sourceURL string, depth int) error {
	if len(pageIDs) == 0 {
		return nil
	}

	return q.Execute(ctx, func(tx *sql.Tx) error {
		// Update job's total task count
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs
			SET total_tasks = total_tasks + $1
			WHERE id = $2
		`, len(pageIDs), jobID)
		if err != nil {
			return fmt.Errorf("failed to update job total tasks: %w", err)
		}

		// Prepare statement for batch insert
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO tasks (
				id, job_id, page_id, path, status, depth, created_at, retry_count,
				source_type, source_url
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`)
		if err != nil {
			return fmt.Errorf("failed to prepare statement: %w", err)
		}
		defer stmt.Close()

		// Insert each task
		now := time.Now()
		for i, pageID := range pageIDs {
			if pageID == 0 {
				continue
			}

			taskID := uuid.New().String()
			_, err = stmt.ExecContext(ctx,
				taskID, jobID, pageID, paths[i], "pending", depth, now, 0, sourceType, sourceURL)

			if err != nil {
				return fmt.Errorf("failed to insert task: %w", err)
			}
		}

		return nil
	})
}

// EnqueueTasks is an alias for EnqueueURLs to maintain compatibility with existing code
func (q *DbQueue) EnqueueTasks(ctx context.Context, jobID string, pageIDs []int, paths []string, sourceType string, sourceURL string, depth int) error {
	return q.EnqueueURLs(ctx, jobID, pageIDs, paths, sourceType, sourceURL, depth)
}

// NewTaskQueue creates a task queue using the provided database connection
// It's an alias for NewDbQueue to maintain compatibility with existing code
func NewTaskQueue(db *sql.DB) *DbQueue {
	return NewDbQueue(db)
}

// CompleteTask marks a task as completed
func (q *DbQueue) CompleteTask(ctx context.Context, task *Task) error {
	// Update task status in a transaction
	if err := q.Execute(ctx, func(tx *sql.Tx) error {
		task.Status = "completed"
		task.CompletedAt = time.Now()
		_, err := tx.ExecContext(ctx, `
			UPDATE tasks 
			SET status = $1, completed_at = $2, status_code = $3, 
				response_time = $4, cache_status = $5, content_type = $6
			WHERE id = $7
		`, task.Status, task.CompletedAt, task.StatusCode,
			task.ResponseTime, task.CacheStatus, task.ContentType, task.ID)
		return err
	}); err != nil {
		return err
	}
	// Update job progress after task update
	if task.JobID != "" {
		return q.UpdateJobProgress(ctx, task.JobID)
	}
	return nil
}

// FailTask marks a task as failed
func (q *DbQueue) FailTask(ctx context.Context, task *Task, err error) error {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}

	_, dbErr := q.db.ExecContext(ctx, `
		UPDATE tasks
		SET 
			status = 'failed', 
			completed_at = $1,
			error = $2
		WHERE id = $3
	`, time.Now(), errMsg, task.ID)

	if dbErr != nil {
		return fmt.Errorf("failed to mark task as failed: %w", dbErr)
	}

	// Update job progress
	return q.UpdateJobProgress(ctx, task.JobID)
}

// UpdateJobProgress updates a job's progress based on task completion
func (q *DbQueue) UpdateJobProgress(ctx context.Context, jobID string) error {
	// Start a transaction
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get counts: use jobs.total_tasks and task statuses
	var totalTasks, compCount, failCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT j.total_tasks,
			   COUNT(*) FILTER (WHERE t.status = 'completed'),
			   COUNT(*) FILTER (WHERE t.status = 'failed')
		FROM jobs j
		LEFT JOIN tasks t ON t.job_id = j.id
		WHERE j.id = $1
		GROUP BY j.total_tasks
	`, jobID).Scan(&totalTasks, &compCount, &failCount); err != nil {
		return fmt.Errorf("failed to get job counts: %w", err)
	}

	// Calculate progress percentage
	var progress float64 = 0.0
	if totalTasks > 0 {
		progress = float64(compCount+failCount) / float64(totalTasks) * 100.0
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE jobs
		SET 
			progress = $1::REAL,
			completed_tasks = $2,
			failed_tasks = $3,
			status = CASE 
				WHEN $1::REAL >= 100.0 THEN 'completed'
				ELSE status
			END,
			completed_at = CASE 
				WHEN $1::REAL >= 100.0 THEN NOW()
				ELSE completed_at
			END
		WHERE id = $4
	`, progress, compCount, failCount, jobID)

	if err != nil {
		return fmt.Errorf("failed to update job progress: %w", err)
	}

	return tx.Commit()
}

// GetNextTask is an alias for GetNextPendingTask for compatibility
func (q *DbQueue) GetNextTask(ctx context.Context) (*Task, error) {
	return q.GetNextPendingTask(ctx, "")
}
