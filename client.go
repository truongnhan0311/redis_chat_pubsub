package chat

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second    // Max time to write a message to the peer.
	pongWait       = 60 * time.Second    // Time allowed to read the next pong from peer.
	pingPeriod     = (pongWait * 9) / 10 // How often we ping. Must be less than pongWait.
	maxMessageSize = 8192                // Max incoming message size in bytes.
)

// Client represents a single connected WebSocket user.
type Client struct {
	hub    *Hub
	conn   *websocket.Conn
	send   chan any // Buffered outbound channel; accepts Message or SystemMessage.
	user   User
	token  string // Reconnect token tied to this session.
	logger *log.Logger
}

// ReadPump reads incoming messages from the WebSocket and dispatches them.
// It runs in its own goroutine per client.
func (c *Client) ReadPump(ctx context.Context) {
	defer func() {
		c.hub.unregister(c)
		c.conn.Close()
		c.logger.Printf("[client] %s (%s) disconnected", c.user.Name, c.user.UUID)

		// Touch session so reconnect window stays alive.
		if sess, ok := c.hub.sessions.get(c.token); ok {
			sess.touch()
		}
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Printf("[client] read error for %s: %v", c.user.UUID, err)
			}
			break
		}

		// Log raw inbound frame.
		c.logger.Printf("[client] ← user=%s raw: %s", c.user.UUID, string(raw))

		var incoming IncomingMessage
		if err := json.Unmarshal(raw, &incoming); err != nil {
			c.logger.Printf("[client] bad message from %s: %v", c.user.UUID, err)
			c.sendError(incoming.ConnID, "invalid JSON: "+err.Error())
			continue
		}

		// Validate required fields before doing any work.
		if err := incoming.Validate(); err != nil {
			c.logger.Printf("[client] invalid message from %s: %v", c.user.UUID, err)
			c.sendError(incoming.ConnID, err.Error())
			continue
		}

		msg, err := c.hub.handleIncoming(ctx, c, incoming)
		if err != nil {
			c.sendError(incoming.ConnID, "internal error: "+err.Error())
		} else {
			c.sendAck(incoming.ConnID, msg)
		}
	}
}

// sendAck pushes a JSON ack frame.
func (c *Client) sendAck(connID string, msg Message) {
	frame := map[string]any{
		"kind": "ack",
		"data": msg,
	}
	if connID != "" {
		frame["conn_id"] = connID
	}
	c.logger.Printf("[client] → user=%s ack=%s", c.user.UUID, msg.ID)
	select {
	case c.send <- frame:
	default: // buffer full
	}
}

// sendError pushes a JSON error frame directly to the client.
// Format: { "kind": "error", "conn_id": "...", "data": { "err": "message" } }
func (c *Client) sendError(connID, errMsg string) {
	frame := map[string]any{
		"kind": "error",
		"data": map[string]string{"err": errMsg},
	}
	if connID != "" {
		frame["conn_id"] = connID
	}
	c.logger.Printf("[client] → user=%s error: %s", c.user.UUID, errMsg)
	select {
	case c.send <- frame:
	default:
		// buffer full — skip error delivery, connection is unhealthy
	}
}

// WritePump writes queued frames to the WebSocket.
// It runs in its own goroutine per client.
// The send channel accepts two frame types:
//   - Message        → regular chat message (server → client)
//   - SystemMessage  → system event (session token, errors, acks)
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case frame, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteJSON(frame); err != nil {
				c.logger.Printf("[client] write error for %s: %v", c.user.UUID, err)
				return
			}
			// Log outbound frame.
			if raw, err := json.Marshal(frame); err == nil {
				c.logger.Printf("[client] → user=%s raw: %s", c.user.UUID, string(raw))
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleIncoming routes a message from a client through the hub.
func (h *Hub) handleIncoming(ctx context.Context, sender *Client, incoming IncomingMessage) (Message, error) {
	// Identity registration — only on FIRST message (when UUID is not yet set).
	// In the API Server → Chat Server model, the API Server sends the verified
	// UUID (from JWT) in the first message. After that, the UUID is LOCKED —
	// clients cannot change their identity mid-session.
	if sender.user.UUID == "" && incoming.UUID != "" {
		h.mu.Lock()
		// Remove the empty-key placeholder if registered.
		delete(h.clients, "")
		// Set real identity.
		sender.user.UUID = incoming.UUID
		if incoming.Name != "" {
			sender.user.Name = incoming.Name
		}
		if incoming.Photo != "" {
			sender.user.PhotoURL = incoming.Photo
		}
		h.clients[sender.user.UUID] = append(h.clients[sender.user.UUID], sender)
		h.mu.Unlock()
		h.logger.Printf("[hub] client identified: uuid=%s name=%s", sender.user.UUID, sender.user.Name)
	}
	// After identity is set, ignore any uuid field in subsequent messages
	// — the connection's identity is fixed for the session lifetime.

	msg := Message{
		Type:        incoming.Type,
		SenderID:    sender.user.UUID,
		SenderName:  sender.user.Name,
		SenderPhoto: sender.user.PhotoURL,
		TargetID:    incoming.TargetID,
		IsGroup:     incoming.IsGroup,
		Content:     incoming.Content,
		Metadata:    incoming.Metadata,
		Timestamp:   time.Now().UTC(),
	}

	// For group messages, hydrate group info.
	if incoming.IsGroup {
		msg.GroupID = incoming.TargetID
		if g, err := h.groups.GetGroup(ctx, incoming.TargetID); err == nil {
			msg.GroupName = g.Name
			msg.GroupPhoto = g.PhotoURL
		}
	}

	streamID, err := h.SaveMessage(ctx, msg)
	if err != nil {
		h.logger.Printf("[hub] failed to save message: %v", err)
		return Message{}, err
	}
	msg.ID = streamID

	// Update session's last-seen pointer.
	if sess, ok := h.sessions.getByUser(sender.user.UUID); ok {
		sess.updateLastSeen(incoming.TargetID, streamID)
		go h.sessions.persist(ctx, sess)
	}

	// 2. Deliver.
	if incoming.IsGroup {
		h.deliverToGroup(ctx, msg)
	} else {
		h.Publish(ctx, incoming.TargetID, msg)
		h.deliverLocally(sender.user.UUID, msg) // echo to sender
	}

	return msg, nil
}

// deliverToGroup fans a message out to all group members.
func (h *Hub) deliverToGroup(ctx context.Context, msg Message) {
	members, err := h.groups.getMembers(ctx, msg.TargetID)
	if err != nil {
		h.logger.Printf("[hub] cannot get group members for %s: %v", msg.TargetID, err)
		return
	}
	for _, memberID := range members {
		if memberID == msg.SenderID {
			// Echo back to sender too.
			h.deliverLocally(memberID, msg)
			continue
		}
		h.Publish(ctx, memberID, msg)
	}
}
