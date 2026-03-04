package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// WAL (Write-Ahead Log) provides on-disk durability for messages.
// It complements Redis so the system can self-recover even if Redis is
// temporarily unavailable or has been restarted without persistence.
//
// Layout: each line in the WAL file is a JSON-serialised WALEntry.
// On startup, the recovery routine reads the WAL and re-publishes any
// messages whose Redis Stream IDs are absent (i.e. Redis was cleared).

const (
	walFlushInterval = 100 * time.Millisecond
	walMaxEntries    = 10_000 // After this many entries, a checkpoint is written.
)

// WALEntry is a single record in the write-ahead log.
type WALEntry struct {
	StreamID  string    `json:"stream_id"` // Redis Stream ID (empty if Redis was down)
	Message   Message   `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// WAL manages an append-only log file for message durability.
type WAL struct {
	mu       sync.Mutex
	file     *os.File
	encoder  *json.Encoder
	filePath string
	count    int
	logger   *log.Logger
}

// NewWAL opens (or creates) a WAL file at the given path.
func NewWAL(filePath string, logger *log.Logger) (*WAL, error) {
	if logger == nil {
		logger = log.New(os.Stdout, "[wal] ", log.LstdFlags)
	}
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open wal file: %w", err)
	}
	return &WAL{
		file:     f,
		encoder:  json.NewEncoder(f),
		filePath: filePath,
		logger:   logger,
	}, nil
}

// Append writes a message to the WAL. It is safe to call from multiple goroutines.
func (w *WAL) Append(streamID string, msg Message) error {
	entry := WALEntry{
		StreamID:  streamID,
		Message:   msg,
		CreatedAt: time.Now().UTC(),
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.encoder.Encode(entry); err != nil {
		return fmt.Errorf("wal encode: %w", err)
	}
	w.count++
	return nil
}

// Close flushes and closes the WAL file.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// Recover reads all WAL entries and calls replayFn for each one.
// Typically replayFn re-publishes messages into Redis if they are missing.
func (w *WAL) Recover(ctx context.Context, replayFn func(entry WALEntry) error) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Seek to the beginning.
	if _, err := w.file.Seek(0, 0); err != nil {
		return 0, fmt.Errorf("wal seek: %w", err)
	}

	decoder := json.NewDecoder(w.file)
	replayed := 0

	for {
		select {
		case <-ctx.Done():
			return replayed, ctx.Err()
		default:
		}

		var entry WALEntry
		if err := decoder.Decode(&entry); err != nil {
			break // EOF or corrupt entry.
		}

		if err := replayFn(entry); err != nil {
			w.logger.Printf("[wal] replay error for msg %s: %v", entry.Message.ID, err)
			continue
		}
		replayed++
	}

	w.logger.Printf("[wal] recovery complete: %d entries replayed", replayed)
	return replayed, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: WAL-backed SaveMessage
// ─────────────────────────────────────────────────────────────────────────────

// WALBacked wraps a ChatModule with WAL durability.
type WALBacked struct {
	module *ChatModule
	wal    *WAL
}

// WithWAL wraps a ChatModule with a WAL for extra durability.
func WithWAL(module *ChatModule, walPath string) (*WALBacked, error) {
	wal, err := NewWAL(walPath, module.logger)
	if err != nil {
		return nil, err
	}
	return &WALBacked{module: module, wal: wal}, nil
}

// SendMessage persists to WAL first, then delegates to ChatModule.
func (wb *WALBacked) SendMessage(ctx context.Context, msg Message) error {
	// 1. Write to WAL before anything else.
	if err := wb.wal.Append("", msg); err != nil {
		wb.module.logger.Printf("[wal-backed] wal append error: %v", err)
		// Non-fatal — continue anyway.
	}

	// 2. Attempt normal delivery.
	return wb.module.SendMessage(ctx, msg)
}

// Recover re-publishes WAL entries on startup.
// Call this once before module.Run(ctx) if you want crash recovery.
func (wb *WALBacked) Recover(ctx context.Context) error {
	_, err := wb.wal.Recover(ctx, func(entry WALEntry) error {
		// Check if this message is already in Redis (by stream ID).
		if entry.StreamID != "" {
			exists, err := wb.module.redis.Exists(ctx,
				conversationStreamKey(entry.Message.SenderID, entry.Message.TargetID, entry.Message.IsGroup),
			).Result()
			if err == nil && exists > 0 {
				return nil // Already persisted, skip.
			}
		}
		return wb.module.SendMessage(ctx, entry.Message)
	})
	return err
}

// Close flushes and closes the WAL.
func (wb *WALBacked) Close() error {
	return wb.wal.Close()
}
