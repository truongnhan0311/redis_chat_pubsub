package chat

import (
	"fmt"
	"time"
)

// MessageType represents the type of content in a message.
type MessageType string

const (
	MessageTypeText  MessageType = "text"
	MessageTypeImage MessageType = "image"
	MessageTypePDF   MessageType = "pdf"
	MessageTypeVoice MessageType = "voice"
	MessageTypeVideo MessageType = "video"
)

// validMessageTypes is the whitelist used during incoming message validation.
var validMessageTypes = map[MessageType]bool{
	MessageTypeText:  true,
	MessageTypeImage: true,
	MessageTypePDF:   true,
	MessageTypeVoice: true,
	MessageTypeVideo: true,
}

// User represents a chat participant.
type User struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	PhotoURL string `json:"photo_url"`
}

// MessageMetadata holds extra info for multimedia messages.
type MessageMetadata struct {
	FileName  string `json:"file_name,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"` // in bytes
	MimeType  string `json:"mime_type,omitempty"`
	Duration  int    `json:"duration,omitempty"`  // seconds (for voice/video)
	Width     int    `json:"width,omitempty"`     // pixels (for image/video)
	Height    int    `json:"height,omitempty"`    // pixels (for image/video)
	Thumbnail string `json:"thumbnail,omitempty"` // preview URL (for video)
}

// ─────────────────────────────────────────────────────────────────────────────
// Wire formats — exactly what JSON flows over the wire at each layer.
// ─────────────────────────────────────────────────────────────────────────────

// IncomingMessage is the JSON payload a CLIENT sends to the App Server
// over the WebSocket connection.
//
// Wire example — PM text message:
//
//	{
//	  "type":      "text",
//	  "target_id": "uuid-of-recipient",
//	  "is_group":  false,
//	  "content":   "Hello!"
//	}
//
// Wire example — Group image message:
//
//	{
//	  "type":      "image",
//	  "target_id": "group-uuid",
//	  "is_group":  true,
//	  "content":   "https://cdn.example.com/photo.jpg",
//	  "metadata":  { "file_name": "photo.jpg", "file_size": 204800, "mime_type": "image/jpeg", "width": 1280, "height": 720 }
//	}
type IncomingMessage struct {
	Type     MessageType     `json:"type"`      // required: text | image | pdf | voice | video
	TargetID string          `json:"target_id"` // required: recipient UUID (PM) or group UUID
	IsGroup  bool            `json:"is_group"`  // true = group chat
	Content  string          `json:"content"`   // required: text body OR media URL (S3/CDN)
	Metadata MessageMetadata `json:"metadata,omitempty"`
}

// Validate checks that the incoming message has all required fields.
func (m *IncomingMessage) Validate() error {
	if !validMessageTypes[m.Type] {
		return fmt.Errorf("unknown message type %q", m.Type)
	}
	if m.TargetID == "" {
		return fmt.Errorf("target_id is required")
	}
	if m.Content == "" {
		return fmt.Errorf("content is required")
	}
	return nil
}

// Message is the fully-hydrated message struct:
//   - Stored in Redis Streams (persistence layer).
//   - Sent SERVER → CLIENT over WebSocket.
//   - Wrapped in pubSubEnvelope for SERVER ↔ SERVER delivery via Redis Pub/Sub.
//
// Wire example (server → client WebSocket):
//
//	{
//	  "id":           "1714000000123-0",
//	  "type":         "text",
//	  "sender_id":    "uuid-of-alice",
//	  "sender_name":  "Alice",
//	  "sender_photo": "https://cdn.example.com/alice.jpg",
//	  "target_id":    "uuid-of-bob",
//	  "is_group":     false,
//	  "content":      "Hello Bob!",
//	  "timestamp":    "2024-05-01T12:00:00.000Z"
//	}
type Message struct {
	ID          string          `json:"id"` // Redis Stream entry ID (e.g. "1714000000123-0")
	Type        MessageType     `json:"type"`
	SenderID    string          `json:"sender_id"`
	SenderName  string          `json:"sender_name"`
	SenderPhoto string          `json:"sender_photo"`
	TargetID    string          `json:"target_id"` // user UUID for PM, group UUID for group
	IsGroup     bool            `json:"is_group"`
	Content     string          `json:"content"` // text body or media URL
	Metadata    MessageMetadata `json:"metadata,omitempty"`
	Timestamp   time.Time       `json:"timestamp"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"` // set when ACK received from recipient
	ReadAt      *time.Time      `json:"read_at,omitempty"`
}

// pubSubEnvelope is the JSON format used for SERVER ↔ SERVER communication
// over Redis Pub/Sub (channel: "chat:pubsub:global").
//
// When App Server 1 receives a message for a user who is connected to
// App Server 2, it publishes this envelope to Redis. App Server 2 receives
// it via its Pub/Sub subscription, extracts the Message, and forwards it
// to the recipient's WebSocket.
//
// Wire example (Redis Pub/Sub payload):
//
//	{
//	  "recipient_id": "uuid-of-bob",
//	  "message": {
//	    "id":          "1714000000123-0",
//	    "type":        "text",
//	    "sender_id":   "uuid-of-alice",
//	    "sender_name": "Alice",
//	    "target_id":   "uuid-of-bob",
//	    "content":     "Hello Bob!",
//	    "timestamp":   "2024-05-01T12:00:00.000Z"
//	  }
//	}
//
// Note: pubSubEnvelope is defined in hub.go where it is used.
// It is documented here for discoverability.

// ─────────────────────────────────────────────────────────────────────────────
// Other event types (client ↔ server)
// ─────────────────────────────────────────────────────────────────────────────

// Group represents a group chat room.
type Group struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	PhotoURL  string    `json:"photo_url"`
	OwnerID   string    `json:"owner_id"`
	MemberIDs []string  `json:"member_ids"`
	CreatedAt time.Time `json:"created_at"`
}

// ChatListEntry is a summary of a conversation shown in the user's chat list.
type ChatListEntry struct {
	ConversationID string    `json:"conversation_id"` // user UUID for PM, group ID for group
	IsGroup        bool      `json:"is_group"`
	Name           string    `json:"name"`
	PhotoURL       string    `json:"photo_url"`
	LastMessage    *Message  `json:"last_message,omitempty"`
	UnreadCount    int       `json:"unread_count"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TypingEvent signals that a user is typing.
// Sent client → server; broadcast server → all members of the conversation.
//
// Wire example:
//
//	{ "sender_id": "uuid-alice", "sender_name": "Alice", "target_id": "uuid-bob", "is_group": false, "is_typing": true }
type TypingEvent struct {
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	TargetID   string `json:"target_id"`
	IsGroup    bool   `json:"is_group"`
	IsTyping   bool   `json:"is_typing"`
}

// AckEvent is sent by the client to confirm a message was received.
//
// Wire example:
//
//	{ "message_id": "1714000000123-0" }
type AckEvent struct {
	MessageID string `json:"message_id"`
}

// SystemMessage is a special message frame the server sends to the client
// to communicate session tokens, errors, or system notices.
//
// Wire example (session token on connect):
//
//	{ "kind": "session", "payload": "a1b2c3d4-..." }
//
// Wire example (error):
//
//	{ "kind": "error", "payload": "invalid target_id" }
type SystemMessage struct {
	Kind    string `json:"kind"` // "session" | "error" | "ack" | "online"
	Payload string `json:"payload"`
}
