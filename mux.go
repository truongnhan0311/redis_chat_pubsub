package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ─────────────────────────────────────────────────────────────────────────────
// MUX Wire Formats
// ─────────────────────────────────────────────────────────────────────────────

// MuxInbound is the envelope the API Server sends to the Chat Server.
// One physical WS connection carries messages for many users.
//
// Wire format:
//
//	{
//	  "user_id":    "alice-001",         ← required: which user is sending
//	  "user_name":  "Alice",             ← optional: display name
//	  "user_photo": "https://...",       ← optional: avatar URL
//	  "data": {                          ← the actual message
//	    "type":      "text",
//	    "target_id": "bob-002",
//	    "is_group":  false,
//	    "content":   "Hello Bob!"
//	  }
//	}
type MuxInbound struct {
	UserID    string          `json:"user_id"` // required
	UserName  string          `json:"user_name,omitempty"`
	UserPhoto string          `json:"user_photo,omitempty"`
	Data      IncomingMessage `json:"data"`
}

// MuxOutbound is the envelope the Chat Server sends back to the API Server.
// The API Server uses user_id to forward the message to the correct client.
//
// Wire format:
//
//	{
//	  "user_id": "bob-002",             ← forward to this client
//	  "kind":    "message",             ← "message" | "session"
//	  "data":    { ...Message... }
//	}
type MuxOutbound struct {
	UserID string `json:"user_id"` // which client to deliver to
	Kind   string `json:"kind"`    // "message" | "session"
	Data   any    `json:"data"`    // Message or map[string]string for session
}

// ─────────────────────────────────────────────────────────────────────────────
// muxGateway — represents ONE physical WS connection from an API Server node
// ─────────────────────────────────────────────────────────────────────────────

// muxGateway handles a single multiplexed WebSocket connection.
// All users routed through this gateway share one physical WS pipe.
// A write mutex ensures frames are never interleaved.
type muxGateway struct {
	id      string // unique ID for this gateway connection
	hub     *Hub
	conn    *websocket.Conn
	writeMu sync.Mutex // only ONE goroutine writes to conn at a time

	mu    sync.RWMutex
	users map[string]bool // userIDs currently registered on this gateway

	logger *log.Logger
}

func newMuxGateway(hub *Hub, conn *websocket.Conn, logger *log.Logger) *muxGateway {
	return &muxGateway{
		id:     uuid.New().String(),
		hub:    hub,
		conn:   conn,
		users:  make(map[string]bool),
		logger: logger,
	}
}

// deliver sends a message to a specific user via this gateway.
// It wraps the message in MuxOutbound so the API Server knows who to forward to.
func (gw *muxGateway) deliver(userID string, kind string, data any) error {
	frame := MuxOutbound{
		UserID: userID,
		Kind:   kind,
		Data:   data,
	}
	gw.writeMu.Lock()
	defer gw.writeMu.Unlock()
	gw.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return gw.conn.WriteJSON(frame)
}

// registerUser marks a user as active on this gateway.
func (gw *muxGateway) registerUser(userID string) {
	gw.mu.Lock()
	gw.users[userID] = true
	gw.mu.Unlock()
}

// unregisterUser removes a single user from this gateway.
func (gw *muxGateway) unregisterUser(userID string) {
	gw.mu.Lock()
	delete(gw.users, userID)
	gw.mu.Unlock()
}

// userIDs returns a snapshot of all user IDs on this gateway.
func (gw *muxGateway) userIDs() []string {
	gw.mu.RLock()
	defer gw.mu.RUnlock()
	ids := make([]string, 0, len(gw.users))
	for id := range gw.users {
		ids = append(ids, id)
	}
	return ids
}

// ─────────────────────────────────────────────────────────────────────────────
// MUX ReadPump & lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// runMux is the main loop for a gateway connection.
// It reads MuxInbound frames and routes them through the hub.
// When the connection closes, ALL users on this gateway are unregistered.
func (gw *muxGateway) runMux(ctx context.Context) {
	defer func() {
		// Gateway disconnected — unregister every user on it.
		gw.hub.unregisterGateway(gw)
		gw.logger.Printf("[mux] gateway %s disconnected, removed %d virtual users",
			gw.id[:8], len(gw.userIDs()))
	}()

	gw.conn.SetReadLimit(maxMessageSize)
	gw.conn.SetReadDeadline(time.Now().Add(pongWait))
	gw.conn.SetPongHandler(func(string) error {
		gw.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Ping goroutine.
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				gw.writeMu.Lock()
				gw.conn.SetWriteDeadline(time.Now().Add(writeWait))
				err := gw.conn.WriteMessage(websocket.PingMessage, nil)
				gw.writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	for {
		_, raw, err := gw.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				gw.logger.Printf("[mux] read error on gateway %s: %v", gw.id[:8], err)
			}
			return
		}

		var inbound MuxInbound
		if err := json.Unmarshal(raw, &inbound); err != nil {
			gw.logger.Printf("[mux] bad frame from gateway %s: %v", gw.id[:8], err)
			continue
		}
		if inbound.UserID == "" {
			gw.logger.Printf("[mux] missing user_id from gateway %s", gw.id[:8])
			continue
		}

		// Register/migrate user onto this gateway.
		gw.hub.registerMuxUser(inbound.UserID, inbound.UserName, inbound.UserPhoto, gw)

		// Validate and route the message.
		msg := inbound.Data
		msg.UUID = inbound.UserID // override UUID — trust the API Server
		if err := msg.Validate(); err != nil {
			gw.logger.Printf("[mux] invalid msg from user %s: %v", inbound.UserID, err)
			continue
		}

		// Find the virtual client and handle as a normal incoming message.
		if client, ok := gw.hub.getMuxClient(inbound.UserID); ok {
			gw.hub.handleIncoming(ctx, client, msg)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Hub MUX methods — added to hub.go via this file
// ─────────────────────────────────────────────────────────────────────────────

// registerMuxUser creates or migrates a virtual client for userID onto gw.
// If the user was on a different gateway, they are moved to the new one.
func (h *Hub) registerMuxUser(userID, userName, userPhoto string, gw *muxGateway) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// If already on a different gateway, remove from old one.
	if old, ok := h.muxUsers[userID]; ok && old != gw {
		old.unregisterUser(userID)
		delete(h.clients, userID) // remove virtual client
		h.logger.Printf("[hub] migrating user %s from gateway %s to %s",
			userID, old.id[:8], gw.id[:8])
	}

	// Create virtual client if not exists.
	if _, exists := h.clients[userID]; !exists {
		vc := &Client{
			hub:    h,
			conn:   nil, // no direct WS — gateway handles transport
			send:   make(chan any, sendBufferSize),
			user:   User{UUID: userID, Name: userName, PhotoURL: userPhoto},
			token:  "", // managed by session store
			logger: h.logger,
		}
		h.clients[userID] = vc

		// Drain the virtual client's send channel onto the gateway WS.
		go h.drainMuxClient(vc, gw)
	}

	h.muxUsers[userID] = gw
	gw.registerUser(userID)
}

// getMuxClient returns the virtual Client for a mux user (read-only).
func (h *Hub) getMuxClient(userID string) (*Client, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.clients[userID]
	return c, ok
}

// unregisterGateway removes all virtual clients belonging to gw.
func (h *Hub) unregisterGateway(gw *muxGateway) {
	userIDs := gw.userIDs()
	h.mu.Lock()
	for _, uid := range userIDs {
		if c, ok := h.clients[uid]; ok {
			close(c.send)
			delete(h.clients, uid)
		}
		delete(h.muxUsers, uid)
	}
	delete(h.gateways, gw.id)
	h.mu.Unlock()
}

// drainMuxClient reads from a virtual client's send channel and forwards
// each frame as a MuxOutbound on the gateway WS.
func (h *Hub) drainMuxClient(vc *Client, gw *muxGateway) {
	for frame := range vc.send {
		var kind string
		switch frame.(type) {
		case Message:
			kind = "message"
		default:
			kind = "session"
		}
		if err := gw.deliver(vc.user.UUID, kind, frame); err != nil {
			h.logger.Printf("[mux] deliver error for user %s: %v", vc.user.UUID, err)
			return
		}
	}
}

// ConnectMux registers a multiplexed gateway WebSocket connection.
// The API Server calls /ws/mux with X-API-Key; one physical connection
// can carry messages for many users simultaneously.
func (cm *ChatModule) ConnectMux(conn *websocket.Conn) {
	ctx := context.Background()
	gw := newMuxGateway(cm.hub, conn, cm.logger)

	cm.hub.mu.Lock()
	cm.hub.gateways[gw.id] = gw
	cm.hub.mu.Unlock()

	cm.slog.Info("mux gateway connected", "id", fmt.Sprintf("%s...", gw.id[:8]))

	gw.runMux(ctx) // blocks until gateway disconnects
}
