package chat

import "time"

// MessageType represents the type of content in a message.
type MessageType string

const (
	MessageTypeText  MessageType = "text"
	MessageTypeImage MessageType = "image"
	MessageTypePDF   MessageType = "pdf"
	MessageTypeVoice MessageType = "voice"
	MessageTypeVideo MessageType = "video"
)

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
	Width     int    `json:"width,omitempty"`     // for image/video
	Height    int    `json:"height,omitempty"`    // for image/video
	Thumbnail string `json:"thumbnail,omitempty"` // preview URL for video
}

// Message is the core unit of communication.
type Message struct {
	ID          string          `json:"id"`
	Type        MessageType     `json:"type"`
	SenderID    string          `json:"sender_id"`
	SenderName  string          `json:"sender_name"`
	SenderPhoto string          `json:"sender_photo"`
	TargetID    string          `json:"target_id"` // user UUID (for PM) or group ID (for group chat)
	IsGroup     bool            `json:"is_group"`
	Content     string          `json:"content"` // text content or URL for media
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
	ConversationID string    `json:"conversation_id"` // user UUID for PM, group ID for group
	IsGroup        bool      `json:"is_group"`
	Name           string    `json:"name"`
	PhotoURL       string    `json:"photo_url"`
	LastMessage    *Message  `json:"last_message,omitempty"`
	UnreadCount    int       `json:"unread_count"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// IncomingMessage is the payload a client sends via WebSocket.
type IncomingMessage struct {
	Type     MessageType     `json:"type"`
	TargetID string          `json:"target_id"`
	IsGroup  bool            `json:"is_group"`
	Content  string          `json:"content"`
	Metadata MessageMetadata `json:"metadata,omitempty"`
}

// SystemEvent is an internal envelope for hub coordination.
type SystemEvent struct {
	Kind    string      `json:"kind"` // "message", "ack", "typing", "online"
	Payload interface{} `json:"payload"`
}

// TypingEvent signals that a user is typing.
type TypingEvent struct {
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	TargetID   string `json:"target_id"`
	IsGroup    bool   `json:"is_group"`
	IsTyping   bool   `json:"is_typing"`
}

// AckEvent is sent by the client to confirm a message was received.
type AckEvent struct {
	MessageID string `json:"message_id"`
}
