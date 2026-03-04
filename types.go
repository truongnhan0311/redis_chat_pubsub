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

// ─────────────────────────────────────────────────────────────────────────────
// MessageMetadata — filled per message type
// ─────────────────────────────────────────────────────────────────────────────
//
// Which fields to populate per type:
//
//	"text"  → all fields are empty/zero
//
//	"image" → file_name, file_size, mime_type ("image/jpeg"|"image/png"|…),
//	           width, height
//	           Example:
//	           { "file_name":"photo.jpg","file_size":204800,
//	             "mime_type":"image/jpeg","width":1280,"height":720 }
//
//	"pdf"   → file_name, file_size, mime_type ("application/pdf"),
//	           page_count
//	           Example:
//	           { "file_name":"report.pdf","file_size":512000,
//	             "mime_type":"application/pdf","page_count":12 }
//
//	"voice" → file_name, file_size, mime_type ("audio/ogg"|"audio/mp4"|…),
//	           duration (seconds)
//	           Example:
//	           { "file_name":"voice.ogg","file_size":32768,
//	             "mime_type":"audio/ogg","duration":15 }
//
//	"video" → file_name, file_size, mime_type ("video/mp4"|…),
//	           width, height, duration (seconds), thumbnail (preview URL)
//	           Example:
//	           { "file_name":"clip.mp4","file_size":5242880,
//	             "mime_type":"video/mp4","width":1920,"height":1080,
//	             "duration":30,"thumbnail":"https://cdn.example.com/thumb.jpg" }
type MessageMetadata struct {
	FileName  string `json:"file_name,omitempty"`  // all media types
	FileSize  int64  `json:"file_size,omitempty"`  // bytes; all media types
	MimeType  string `json:"mime_type,omitempty"`  // all media types
	Duration  int    `json:"duration,omitempty"`   // seconds: voice, video
	Width     int    `json:"width,omitempty"`      // pixels: image, video
	Height    int    `json:"height,omitempty"`     // pixels: image, video
	Thumbnail string `json:"thumbnail,omitempty"`  // CDN URL: video preview
	PageCount int    `json:"page_count,omitempty"` // pdf only
}

// ─────────────────────────────────────────────────────────────────────────────
// Wire formats
// ─────────────────────────────────────────────────────────────────────────────

// IncomingMessage is the JSON payload a CLIENT sends to the App Server
// over its WebSocket connection.
//
// The FIRST message a client sends after opening the WebSocket connection
// MUST include uuid (and optionally name/photo/token) to identify the user
// and optionally provide a reconnect token to resume a previous session.
// The server expects this within 10 seconds or the connection is closed.
//
// Server responds with a SystemMessage frame:
//
//	{ "kind": "session", "payload": "<session-token>", "uuid": "alice-001" }
//
// ── First message (identity) ──────────────────────────────────────────────────
//
//	{
//	  "uuid":  "alice-001",            ← who is this user
//	  "name":  "Alice",                ← display name (optional)
//	  "photo": "https://cdn/alice.jpg",← avatar (optional)
//	  "token": "prev-session-token"    ← reconnect token (empty = new session)
//	}
//
// ── Subsequent messages (PM) ──────────────────────────────────────────────────
//
//	{
//	  "type":      "text",
//	  "target_id": "<recipient-user-uuid>",
//	  "is_group":  false,
//	  "content":   "Hello Bob!"
//	}
//
// ── Group message ─────────────────────────────────────────────────────────────
//
//	{
//	  "type":      "text",
//	  "target_id": "<group-uuid>",
//	  "is_group":  true,
//	  "content":   "Hey everyone!"
//	}
//
// ── Multimedia (image example) ────────────────────────────────────────────────
//
//	{
//	  "type":      "image",
//	  "target_id": "<uuid>",
//	  "is_group":  false,
//	  "content":   "https://cdn.example.com/photo.jpg",
//	  "metadata":  { "file_name":"photo.jpg","file_size":204800,
//	                 "mime_type":"image/jpeg","width":1280,"height":720 }
//	}
//
// IncomingMessage is every message a client sends over WebSocket.
// Each message carries the sender's uuid — no separate init/handshake needed.
//
// Wire format (PM):
//
//	{
//	  "uuid":      "alice-001",   ← sender's user UUID (required)
//	  "name":      "Alice",       ← sender display name (optional, updates on each send)
//	  "type":      "text",
//	  "target_id": "bob-001",     ← recipient UUID (PM) or group UUID
//	  "is_group":  false,
//	  "content":   "Hello!"
//	}
//
// Wire format (group):
//
//	{
//	  "uuid":      "alice-001",
//	  "type":      "text",
//	  "target_id": "<group-uuid>",
//	  "is_group":  true,
//	  "content":   "Hey everyone!"
//	}
type IncomingMessage struct {
	UUID     string          `json:"uuid"`               // required: sender's user UUID
	Name     string          `json:"name,omitempty"`     // optional: display name
	Photo    string          `json:"photo,omitempty"`    // optional: avatar URL
	Type     MessageType     `json:"type"`               // required: text|image|pdf|voice|video
	TargetID string          `json:"target_id"`          // required: recipient UUID or group UUID
	IsGroup  bool            `json:"is_group,omitempty"` // true = group message
	Content  string          `json:"content"`            // required: text or media URL
	Metadata MessageMetadata `json:"metadata,omitempty"`
}

// Validate returns an error if any required field is missing or invalid.
func (m *IncomingMessage) Validate() error {
	if m.UUID == "" {
		return fmt.Errorf("uuid is required")
	}
	if !validMessageTypes[m.Type] {
		return fmt.Errorf("unknown type %q (valid: text|image|pdf|voice|video)", m.Type)
	}
	if m.TargetID == "" {
		return fmt.Errorf("target_id is required")
	}
	if m.Content == "" {
		return fmt.Errorf("content is required")
	}
	return nil
}

// Message is the fully-hydrated message struct sent SERVER → CLIENT.
// It is also the payload stored in Redis Streams and wrapped in
// pubSubEnvelope for SERVER ↔ SERVER delivery.
//
// ── PM example (server → client) ─────────────────────────────────────────────
//
//	{
//	  "id":           "1714000000123-0",
//	  "type":         "text",
//	  "sender_id":    "<alice-uuid>",
//	  "sender_name":  "Alice",
//	  "sender_photo": "https://cdn.example.com/alice.jpg",
//	  "target_id":    "<bob-uuid>",
//	  "is_group":     false,
//	  "content":      "Hello Bob!",
//	  "timestamp":    "2024-05-01T12:00:00.000Z"
//	}
//
// ── Group example (server → client) ──────────────────────────────────────────
//
//	{
//	  "id":           "1714000000456-0",
//	  "type":         "image",
//	  "sender_id":    "<alice-uuid>",
//	  "sender_name":  "Alice",
//	  "sender_photo": "https://cdn.example.com/alice.jpg",
//	  "target_id":    "<group-uuid>",
//	  "group_id":     "<group-uuid>",
//	  "group_name":   "Team Chat",
//	  "group_photo":  "https://cdn.example.com/team.jpg",
//	  "is_group":     true,
//	  "content":      "https://cdn.example.com/photo.jpg",
//	  "metadata":     { "file_name":"photo.jpg","file_size":204800,
//	                    "mime_type":"image/jpeg","width":1280,"height":720 },
//	  "timestamp":    "2024-05-01T12:01:00.000Z"
//	}
type Message struct {
	ID          string      `json:"id"` // Redis Stream entry ID
	Type        MessageType `json:"type"`
	SenderID    string      `json:"sender_id"`
	SenderName  string      `json:"sender_name"`
	SenderPhoto string      `json:"sender_photo"`
	TargetID    string      `json:"target_id"` // user UUID (PM) or group UUID
	IsGroup     bool        `json:"is_group"`
	// Group info — populated for group messages so the recipient can render
	// the conversation header without an extra API call.
	GroupID     string          `json:"group_id,omitempty"`
	GroupName   string          `json:"group_name,omitempty"`
	GroupPhoto  string          `json:"group_photo,omitempty"`
	Content     string          `json:"content"`
	Metadata    MessageMetadata `json:"metadata,omitempty"`
	Timestamp   time.Time       `json:"timestamp"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
	ReadAt      *time.Time      `json:"read_at,omitempty"`
}

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
	ConversationID string    `json:"conversation_id"` // user UUID for PM, group UUID for group
	IsGroup        bool      `json:"is_group"`
	Name           string    `json:"name"`
	PhotoURL       string    `json:"photo_url"`
	LastMessage    *Message  `json:"last_message,omitempty"`
	UnreadCount    int       `json:"unread_count"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ─────────────────────────────────────────────────────────────────────────────
// System / control frames (server → client only)
// ─────────────────────────────────────────────────────────────────────────────

// SystemMessage is a special frame the server sends to convey non-chat events.
// Clients identify it by checking msg.SenderID == "SYSTEM".
//
//	kinds:
//	  "session"  → payload is the reconnect token string
//	  "error"    → payload is a human-readable error message
//	  "ack"      → payload is the message ID that was delivered
//	  "online"   → payload is a user UUID that came online
//	  "offline"  → payload is a user UUID that went offline
//
// Wire example (token on connect):
//
//	{ "kind": "session", "payload": "a1b2c3d4-…" }
type SystemMessage struct {
	Kind    string `json:"kind"`
	Payload string `json:"payload"`
}

// TypingEvent is broadcast to conversation members when a user starts/stops typing.
//
// Wire example:
//
//	{ "sender_id":"<uuid>","sender_name":"Alice","target_id":"<uuid>","is_group":false,"is_typing":true }
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
