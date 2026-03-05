package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/go-redis/redis/v8"
)

const (
	pubSubChannel  = "chat:pubsub:global"
	sendBufferSize = 256
)

type pubSubEnvelope struct {
	RecipientID string  `json:"recipient_id"`
	Message     Message `json:"message"`
}

// Hub is the central coordinator.
// It tracks both direct WebSocket clients and virtual mux clients,
// supports multi-device (same user_id on multiple connections),
// and fans out messages to all devices of a user.
type Hub struct {
	// Direct WebSocket clients (client → chat server).
	// Slice supports multi-device: same user on phone + laptop = 2 entries.
	clients map[string][]*Client // userUUID → []*Client

	// Mux gateway connections (API Server → chat server).
	gateways    map[string]*muxGateway // gatewayID → muxGateway
	muxConns    map[string]*Client     // connID → virtual Client
	muxGateways map[string]*muxGateway // connID → which gateway it's on
	userConns   map[string][]string    // userID → []connID (all devices for this user)

	mu       sync.RWMutex
	redis    *redis.Client
	sessions *sessionManager
	groups   *groupManager
	logger   *log.Logger
}

func newHub(rdb *redis.Client, sm *sessionManager, gm *groupManager, logger *log.Logger) *Hub {
	return &Hub{
		clients:     make(map[string][]*Client),
		gateways:    make(map[string]*muxGateway),
		muxConns:    make(map[string]*Client),
		muxGateways: make(map[string]*muxGateway),
		userConns:   make(map[string][]string),
		redis:       rdb,
		sessions:    sm,
		groups:      gm,
		logger:      logger,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Direct WS client registration
// ─────────────────────────────────────────────────────────────────────────────

// register adds a direct WS client. Multiple clients per user supported (multi-device).
func (h *Hub) register(client *Client) {
	h.mu.Lock()
	h.clients[client.user.UUID] = append(h.clients[client.user.UUID], client)
	h.mu.Unlock()
}

// unregister removes a specific direct WS client (called on disconnect).
func (h *Hub) unregister(client *Client) {
	h.mu.Lock()
	list := h.clients[client.user.UUID]
	updated := list[:0]
	for _, c := range list {
		if c != client {
			updated = append(updated, c)
		}
	}
	if len(updated) == 0 {
		delete(h.clients, client.user.UUID)
	} else {
		h.clients[client.user.UUID] = updated
	}
	close(client.send)
	h.mu.Unlock()
}

// ─────────────────────────────────────────────────────────────────────────────
// Pub/Sub run loop
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) Run(ctx context.Context) {
	pubsub := h.redis.Subscribe(ctx, pubSubChannel)
	defer pubsub.Close()

	h.logger.Printf("[hub] subscribed to Redis channel: %s", pubSubChannel)

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			h.logger.Println("[hub] shutting down")
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var env pubSubEnvelope
			if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
				h.logger.Printf("[hub] bad pubsub payload: %v", err)
				continue
			}
			h.deliverLocally(env.RecipientID, env.Message)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Delivery
// ─────────────────────────────────────────────────────────────────────────────

// Publish delivers a message to a recipient — locally first, then via Redis Pub/Sub.
func (h *Hub) Publish(ctx context.Context, recipientID string, msg Message) error {
	if delivered := h.deliverLocally(recipientID, msg); delivered {
		return nil
	}
	env := pubSubEnvelope{RecipientID: recipientID, Message: msg}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	return h.redis.Publish(ctx, pubSubChannel, data).Err()
}

// deliverLocally fans out a message to ALL connections for recipientID:
//   - All direct WS clients (phone + laptop both connected directly)
//   - All mux virtual clients (phone via API node 1, laptop via API node 2)
//
// Returns true if delivered to at least one connection.
func (h *Hub) deliverLocally(recipientID string, msg Message) bool {
	delivered := false

	// ── Direct WS clients (fan-out to all devices) ───────────────────────────
	h.mu.RLock()
	directClients := make([]*Client, len(h.clients[recipientID]))
	copy(directClients, h.clients[recipientID])
	h.mu.RUnlock()

	var directBroken []*Client
	for _, c := range directClients {
		select {
		case c.send <- msg:
			delivered = true
		default:
			directBroken = append(directBroken, c)
		}
	}
	for _, c := range directBroken {
		h.logger.Printf("[hub] direct client %s buffer full, disconnecting", recipientID)
		h.unregister(c)
	}

	// ── Mux virtual clients (fan-out to all devices) ─────────────────────────
	if h.deliverToMuxUser(recipientID, msg) {
		delivered = true
	}

	return delivered
}

// ─────────────────────────────────────────────────────────────────────────────
// Online / IsOnline
// ─────────────────────────────────────────────────────────────────────────────

// OnlineUsers returns user UUIDs currently connected to this node (direct + mux).
func (h *Hub) OnlineUsers() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	seen := make(map[string]bool)
	for uuid := range h.clients {
		seen[uuid] = true
	}
	for userID := range h.userConns {
		seen[userID] = true
	}
	uuids := make([]string, 0, len(seen))
	for uuid := range seen {
		uuids = append(uuids, uuid)
	}
	return uuids
}

// IsOnline checks if a user has at least one active connection on this node.
func (h *Hub) IsOnline(userUUID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if list, ok := h.clients[userUUID]; ok && len(list) > 0 {
		return true
	}
	_, hasMux := h.userConns[userUUID]
	return hasMux
}
