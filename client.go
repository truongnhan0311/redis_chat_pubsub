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
	send   chan Message // Buffered outbound channel (graceful degradation).
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

		var incoming IncomingMessage
		if err := json.Unmarshal(raw, &incoming); err != nil {
			c.logger.Printf("[client] bad message from %s: %v", c.user.UUID, err)
			continue
		}

		c.hub.handleIncoming(ctx, c, incoming)
	}
}

// WritePump writes queued messages to the WebSocket.
// It runs in its own goroutine per client.
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteJSON(msg); err != nil {
				c.logger.Printf("[client] write error for %s: %v", c.user.UUID, err)
				return
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
func (h *Hub) handleIncoming(ctx context.Context, sender *Client, incoming IncomingMessage) {
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

	// 1. Persist to Redis Stream first (Write-Ahead pattern).
	streamID, err := h.SaveMessage(ctx, msg)
	if err != nil {
		h.logger.Printf("[hub] failed to save message: %v", err)
		return
	}
	msg.ID = streamID

	// Update sender's session with the latest stream ID.
	if sess, ok := h.sessions.getByUser(sender.user.UUID); ok {
		convKey := incoming.TargetID
		sess.updateLastSeen(convKey, streamID)
	}

	// 2. Deliver to recipients.
	if incoming.IsGroup {
		h.deliverToGroup(ctx, msg)
	} else {
		// PM: deliver to recipient and echo back to sender.
		h.Publish(ctx, incoming.TargetID, msg)
		h.deliverLocally(sender.user.UUID, msg) // Echo to sender for acknowledgement.
	}
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
