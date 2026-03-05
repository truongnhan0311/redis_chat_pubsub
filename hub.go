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
	sendBufferSize = 256 // Buffered channel size per client (graceful degradation)
)

// pubSubEnvelope wraps a message for cross-server delivery via Redis Pub/Sub.
type pubSubEnvelope struct {
	RecipientID string  `json:"recipient_id"` // UUID of the intended recipient
	Message     Message `json:"message"`
}

// Hub is the central coordinator:
//   - Tracks all WebSocket clients (direct + mux virtual) on this server node.
//   - Subscribes to Redis Pub/Sub to receive messages from other nodes.
//   - Delivers messages locally or publishes them for cross-node delivery.
type Hub struct {
	clients  map[string]*Client     // userUUID → Client (direct WS or virtual mux)
	gateways map[string]*muxGateway // gatewayID → muxGateway (API Server connections)
	muxUsers map[string]*muxGateway // userUUID → which gateway they are on
	mu       sync.RWMutex
	redis    *redis.Client
	sessions *sessionManager
	groups   *groupManager
	logger   *log.Logger
}

// newHub constructs a Hub. Call Hub.Run(ctx) in a goroutine to start processing.
func newHub(rdb *redis.Client, sm *sessionManager, gm *groupManager, logger *log.Logger) *Hub {
	return &Hub{
		clients:  make(map[string]*Client),
		gateways: make(map[string]*muxGateway),
		muxUsers: make(map[string]*muxGateway),
		redis:    rdb,
		sessions: sm,
		groups:   gm,
		logger:   logger,
	}
}

// register adds a client to the hub.
func (h *Hub) register(client *Client) {
	h.mu.Lock()
	h.clients[client.user.UUID] = client
	h.mu.Unlock()
}

// unregister removes a client from the hub (called on disconnect).
func (h *Hub) unregister(client *Client) {
	h.mu.Lock()
	if c, ok := h.clients[client.user.UUID]; ok && c == client {
		delete(h.clients, client.user.UUID)
		close(client.send)
	}
	h.mu.Unlock()
}

// Run subscribes to Redis Pub/Sub and dispatches incoming cross-node messages.
// Must be run in its own goroutine.
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

// Publish sends a message to a recipient. If the recipient is connected to
// this node, the message is delivered directly; otherwise it is published to
// Redis so another node can deliver it.
func (h *Hub) Publish(ctx context.Context, recipientID string, msg Message) error {
	// Fast path: recipient is on this node.
	if delivered := h.deliverLocally(recipientID, msg); delivered {
		return nil
	}

	// Slow path: publish to Redis for cross-node delivery.
	env := pubSubEnvelope{RecipientID: recipientID, Message: msg}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	return h.redis.Publish(ctx, pubSubChannel, data).Err()
}

// deliverLocally tries to push a message to a locally connected client.
// Returns true if the client was found and the message was queued.
func (h *Hub) deliverLocally(recipientID string, msg Message) bool {
	h.mu.RLock()
	client, ok := h.clients[recipientID]
	h.mu.RUnlock()

	if !ok {
		return false
	}

	select {
	case client.send <- msg:
		return true
	default:
		// Client's send buffer is full — considered unhealthy, disconnect it.
		h.logger.Printf("[hub] client %s send buffer full, disconnecting", recipientID)
		h.unregister(client)
		return false
	}
}

// OnlineUsers returns the list of UUIDs currently connected to this node.
func (h *Hub) OnlineUsers() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	uuids := make([]string, 0, len(h.clients))
	for uuid := range h.clients {
		uuids = append(uuids, uuid)
	}
	return uuids
}

// IsOnline checks if a user is connected to this specific node.
func (h *Hub) IsOnline(userUUID string) bool {
	h.mu.RLock()
	_, ok := h.clients[userUUID]
	h.mu.RUnlock()
	return ok
}
