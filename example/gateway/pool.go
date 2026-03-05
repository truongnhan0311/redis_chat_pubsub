// Package gateway implements the API Server ↔ Chat Server connection pool.
//
// Architecture:
//
//	Client Apps ──WS──▶ API Server (this package)
//	                        │
//	                   Pool of N WS connections (always maintained at max)
//	                        │
//	                        ▼
//	                   Chat Server (/ws/mux)
//
// Each physical WS in the pool carries messages for many users (MUX).
// When a connection drops, affected users are migrated to healthy connections.
// The pool auto-reconnects to stay at N connections at all times.
package main

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ─────────────────────────────────────────────────────────────────────────────
// Wire types (must match Chat Server's mux.go)
// ─────────────────────────────────────────────────────────────────────────────

// MuxInbound is what this gateway sends TO the Chat Server.
type MuxInbound struct {
	UserID    string          `json:"user_id"`
	UserName  string          `json:"user_name,omitempty"`
	UserPhoto string          `json:"user_photo,omitempty"`
	Data      json.RawMessage `json:"data"` // IncomingMessage JSON
}

// MuxOutbound is what this gateway receives FROM the Chat Server.
type MuxOutbound struct {
	UserID string          `json:"user_id"` // which client to deliver to
	Kind   string          `json:"kind"`    // "message" | "session"
	Data   json.RawMessage `json:"data"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Pool
// ─────────────────────────────────────────────────────────────────────────────

// DeliverFunc is called by the pool when the Chat Server sends a message
// to a user. The API Server should forward this to the correct client WS.
type DeliverFunc func(userID string, kind string, data json.RawMessage)

// PoolConfig holds the pool configuration.
type PoolConfig struct {
	ChatServerURL string        // e.g. "ws://localhost:8080/ws/mux"
	APIKey        string        // X-API-Key for the Chat Server
	Size          int           // number of WS connections in the pool (e.g. 10)
	ReconnectWait time.Duration // base wait before reconnect (default: 1s)
	OnDeliver     DeliverFunc   // called when Chat Server delivers a message
	Logger        *slog.Logger
}

// Pool manages a fixed-size pool of multiplexed WebSocket connections
// to the Chat Server. It is safe for concurrent use.
type Pool struct {
	cfg      PoolConfig
	slots    []*slot        // one per pool position
	userSlot map[string]int // userID → slot index
	mu       sync.RWMutex
}

// slot represents one physical WS connection in the pool.
type slot struct {
	idx    int
	pool   *Pool
	mu     sync.Mutex // write mutex — one writer at a time
	conn   *websocket.Conn
	users  map[string]bool // users on this slot
	alive  bool
	userMu sync.RWMutex
}

// NewPool creates a pool and immediately starts connecting.
func NewPool(ctx context.Context, cfg PoolConfig) *Pool {
	if cfg.Size <= 0 {
		cfg.Size = 10
	}
	if cfg.ReconnectWait <= 0 {
		cfg.ReconnectWait = time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	p := &Pool{
		cfg:      cfg,
		slots:    make([]*slot, cfg.Size),
		userSlot: make(map[string]int),
	}

	for i := 0; i < cfg.Size; i++ {
		s := &slot{idx: i, pool: p, users: make(map[string]bool)}
		p.slots[i] = s
		go p.maintainSlot(ctx, s) // keeps this slot alive forever
	}

	return p
}

// ─────────────────────────────────────────────────────────────────────────────
// Connection maintenance — auto-reconnect loop per slot
// ─────────────────────────────────────────────────────────────────────────────

// maintainSlot keeps slot s connected at all times.
// If the connection breaks, it reconnects and re-registers all affected users.
func (p *Pool) maintainSlot(ctx context.Context, s *slot) {
	wait := p.cfg.ReconnectWait
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := p.dial(ctx)
		if err != nil {
			p.cfg.Logger.Warn("pool: dial failed, retrying",
				"slot", s.idx, "err", err, "wait", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				wait = min(wait*2, 30*time.Second) // exponential backoff, max 30s
			}
			continue
		}

		wait = p.cfg.ReconnectWait // reset backoff on success

		s.mu.Lock()
		s.conn = conn
		s.alive = true
		s.mu.Unlock()

		p.cfg.Logger.Info("pool: slot connected", "slot", s.idx)

		// Re-register users still assigned to this slot after reconnect.
		// IMPORTANT: only re-claim users where userSlot[uid] == s.idx.
		// Users that migrated away (userSlot[uid] != s.idx) should NOT be
		// pulled back — they are being served by another healthy slot.
		s.userMu.RLock()
		affected := make([]string, 0, len(s.users))
		for uid := range s.users {
			p.mu.RLock()
			assignedSlot := p.userSlot[uid]
			p.mu.RUnlock()
			if assignedSlot == s.idx { // only re-claim if still assigned here
				affected = append(affected, uid)
			}
		}
		s.userMu.RUnlock()

		for _, uid := range affected {
			p.sendOnSlot(s, uid, "", "", nil) // re-register with empty data
		}

		// Read loop — blocks until connection closes.
		p.readLoop(ctx, s, conn)

		// Connection broke.
		s.mu.Lock()
		s.alive = false
		s.conn = nil
		s.mu.Unlock()

		p.cfg.Logger.Warn("pool: slot disconnected, reconnecting", "slot", s.idx)
	}
}

func (p *Pool) dial(ctx context.Context) (*websocket.Conn, error) {
	header := http.Header{"X-API-Key": {p.cfg.APIKey}}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, p.cfg.ChatServerURL, header)
	return conn, err
}

// readLoop reads MuxOutbound frames from the Chat Server and calls OnDeliver.
func (p *Pool) readLoop(ctx context.Context, s *slot, conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return // triggers reconnect in maintainSlot
		}
		var out MuxOutbound
		if err := json.Unmarshal(raw, &out); err != nil {
			p.cfg.Logger.Warn("pool: bad outbound frame", "err", err)
			continue
		}
		if p.cfg.OnDeliver != nil {
			p.cfg.OnDeliver(out.UserID, out.Kind, out.Data)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Send
// ─────────────────────────────────────────────────────────────────────────────

// Send routes a message from userID to the Chat Server via the assigned slot.
// userID is assigned to a slot deterministically (consistent hash).
// If the assigned slot is down, it finds the next healthy slot.
func (p *Pool) Send(userID, userName, userPhoto string, msgJSON json.RawMessage) error {
	s := p.slotFor(userID)

	// Track user → slot mapping.
	p.mu.Lock()
	p.userSlot[userID] = s.idx
	p.mu.Unlock()

	s.userMu.Lock()
	s.users[userID] = true
	s.userMu.Unlock()

	return p.sendOnSlot(s, userID, userName, userPhoto, msgJSON)
}

// sendOnSlot writes a MuxInbound frame on slot s.
func (p *Pool) sendOnSlot(s *slot, userID, userName, userPhoto string, msgJSON json.RawMessage) error {
	frame := MuxInbound{
		UserID:    userID,
		UserName:  userName,
		UserPhoto: userPhoto,
		Data:      msgJSON,
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil // slot reconnecting, message dropped (client will replay on reconnect)
	}
	s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return s.conn.WriteMessage(websocket.TextMessage, raw)
}

// slotFor returns the slot for a userID using consistent hash % pool size.
// Falls back to any healthy slot if the assigned one is down.
func (p *Pool) slotFor(userID string) *slot {
	h := fnv.New32a()
	h.Write([]byte(userID))
	idx := int(h.Sum32()) % len(p.slots)

	s := p.slots[idx]
	if s.alive {
		return s
	}
	// fallback: find next healthy slot
	for i := 1; i < len(p.slots); i++ {
		alt := p.slots[(idx+i)%len(p.slots)]
		if alt.alive {
			return alt
		}
	}
	return s // all down, return original (will buffer/drop)
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
