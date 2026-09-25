// Package models holds the plain data types that flow through the system.
//
// These structs deliberately depend on nothing (no database, no HTTP). They are
// the shared vocabulary that store, api, worker, and dashboard all speak. Keeping
// them dependency-free means we can pass a Task around freely without dragging
// in a database handle or a JSON encoder's quirks.
package models

import (
	"encoding/json"
	"time"
)

// TaskStatus is the lifecycle state of a task. Mirrors the CHECK constraint in
// the database (migration 0001). We use typed constants instead of bare strings
// so a typo like "complete" (missing d) is a compile-time-ish error rather than
// a silent bug — you reference StatusCompleted, not "completed".
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"   // waiting to be claimed
	StatusRunning   TaskStatus = "running"   // claimed, in flight
	StatusCompleted TaskStatus = "completed" // succeeded (terminal)
	StatusFailed    TaskStatus = "failed"    // one attempt failed
	StatusDead      TaskStatus = "dead"      // retries exhausted (terminal)
)

// WorkerStatus mirrors the workers.status CHECK constraint.
type WorkerStatus string

const (
	WorkerActive   WorkerStatus = "active"
	WorkerDraining WorkerStatus = "draining"
	WorkerDead     WorkerStatus = "dead"
)

// Task is one unit of work. Field tags control the JSON shape returned by the
// API. Pointer fields (*time.Time, *string) are used for genuinely-nullable
// columns so "not set" is distinguishable from a zero value in JSON.
type Task struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	Status      TaskStatus      `json:"status"`
	Priority    int             `json:"priority"`
	ScheduledAt time.Time       `json:"scheduled_at"`
	LockedBy    *string         `json:"locked_by,omitempty"`
	LockedAt    *time.Time      `json:"locked_at,omitempty"`
	RetryCount  int             `json:"retry_count"`
	MaxRetries  int             `json:"max_retries"`
	LastError   *string         `json:"last_error,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
}

// ClaimedTask is the trimmed-down view a worker receives when it claims work.
// The claim query only returns the columns a handler actually needs, which keeps
// the hot path lean.
type ClaimedTask struct {
	ID         string
	Type       string
	Payload    json.RawMessage
	RetryCount int
	MaxRetries int
}

// Worker is a live worker process's health record.
type Worker struct {
	ID             string       `json:"id"`
	Hostname       string       `json:"hostname"`
	Status         WorkerStatus `json:"status"`
	LastHeartbeat  time.Time    `json:"last_heartbeat"`
	TasksCompleted int64        `json:"tasks_completed"`
	TasksFailed    int64        `json:"tasks_failed"`
	StartedAt      time.Time    `json:"started_at"`
}

// DeadLetter is a task that exhausted its retries — a snapshot captured at the
// moment of death.
type DeadLetter struct {
	ID          string          `json:"id"`
	TaskID      string          `json:"task_id"`
	TaskType    string          `json:"task_type"`
	Payload     json.RawMessage `json:"payload"`
	FinalError  *string         `json:"final_error,omitempty"`
	RetryCount  int             `json:"retry_count"`
	ExhaustedAt time.Time       `json:"exhausted_at"`
}

// StatusCounts is the "how many tasks in each state" summary used by the API
// and dashboard. Keyed by status string for easy JSON serialization.
type StatusCounts map[string]int64
