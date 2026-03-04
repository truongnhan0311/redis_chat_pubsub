package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Config holds all options required to initialise the chat module.
type Config struct {
	// Redis connection options.
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// APIKeys is the list of valid API keys for server-to-server authentication.
	// Clients must send one of these keys via the X-API-Key header or ?api_key=
	// query parameter. Leave empty to disable authentication (development only).
	APIKeys []string

	// Logger is the standard library logger (used internally as a fallback).
	// If nil, one is derived from SlogLogger or created pointing to os.Stdout.
	Logger *log.Logger

	// SlogLogger is the structured logger. If nil, a text-format info-level
	// logger writing to os.Stdout is created automatically.
	SlogLogger *slog.Logger

	// AllowedOriginFn overrides the WebSocket origin check.
	// Return true to accept the connection. Defaults to allowing all origins.
	AllowedOriginFn func(r *http.Request) bool
}

// ChatModule is the public facade of the package.
// It exposes everything an application server needs to integrate chat.
// Use cm.Slog() to access the structured logger from calling code.
type ChatModule struct {
	hub      *Hub
	sessions *sessionManager
	groups   *groupManager
	upgrader websocket.Upgrader
	logger   *log.Logger
	slog     *slog.Logger
	redis    *redis.Client
	apiKeys  *apiKeyStore
}

// New initialises the chat module, validates the Redis connection, and returns
// a ready-to-use ChatModule. Call module.Run(ctx) in a goroutine to start
// background processing (Redis subscription, session cleanup, etc.).
func New(cfg Config) (*ChatModule, error) {
	// Build structured logger first.
	sl := cfg.SlogLogger
	if sl == nil {
		sl = NewSlogLogger("", "") // defaults: info level, text format
	}

	// Derive a standard log.Logger from slog if not explicitly provided.
	logger := cfg.Logger
	if logger == nil {
		logger = slog.NewLogLogger(sl.Handler(), slog.LevelInfo)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	originFn := cfg.AllowedOriginFn
	if originFn == nil {
		originFn = func(r *http.Request) bool { return true }
	}

	sm := newSessionManager(rdb)
	gm := newGroupManager(rdb)
	hub := newHub(rdb, sm, gm, logger)

	return &ChatModule{
		hub:      hub,
		sessions: sm,
		groups:   gm,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     originFn,
		},
		logger:  logger,
		slog:    sl,
		redis:   rdb,
		apiKeys: newAPIKeyStore(cfg.APIKeys),
	}, nil
}

// Run starts the hub's Redis Pub/Sub listener and the session cleanup loop.
// This MUST be called in a goroutine: go module.Run(ctx).
func (cm *ChatModule) Run(ctx context.Context) {
	go cm.sessions.cleanup(ctx)
	cm.hub.Run(ctx) // Blocks until ctx is cancelled.
}

// ─────────────────────────────────────────────────────────────────────────────
// WebSocket Connection
// ─────────────────────────────────────────────────────────────────────────────

// ConnectOptions carries parameters for establishing a WebSocket connection.
type ConnectOptions struct {
	// User is required on first connect.
	User User

	// ReconnectToken: pass the token from a previous session to resume.
	// Leave empty for a brand-new connection.
	ReconnectToken string

	// HistoryConversations lists conversation IDs to replay missing messages for.
	// Maps conversationID → isGroup.
	HistoryConversations map[string]bool
}

// UpgradeAndConnect upgrades an HTTP request to WebSocket, then calls Connect.
// Use this as a drop-in net/http handler.
//
// Usage:
//
//	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
//	    user := chat.User{UUID: ..., Name: ..., PhotoURL: ...}
//	    opts := chat.ConnectOptions{User: user}
//	    chatModule.UpgradeAndConnect(w, r, opts)
//	})
func (cm *ChatModule) UpgradeAndConnect(w http.ResponseWriter, r *http.Request, opts ConnectOptions) {
	conn, err := cm.upgrader.Upgrade(w, r, nil)
	if err != nil {
		cm.logger.Printf("[chat] upgrade error: %v", err)
		return
	}
	cm.Connect(conn, opts)
}

// Connect registers a WebSocket connection with the hub, replays history for
// reconnecting users, and starts the read/write pumps.
func (cm *ChatModule) Connect(conn *websocket.Conn, opts ConnectOptions) {
	ctx := context.Background()

	var sess *SessionInfo
	var token string

	// Reconnect path.
	if opts.ReconnectToken != "" {
		if s, ok := cm.sessions.get(opts.ReconnectToken); ok {
			sess = s
			token = opts.ReconnectToken
			// Restore user info from session if not provided.
			if opts.User.UUID == "" {
				opts.User = User{
					UUID:     sess.UserUUID,
					Name:     sess.UserName,
					PhotoURL: sess.UserPhoto,
				}
			}
			cm.logger.Printf("[chat] user %s reconnected (token %s)", sess.UserUUID, token)
		}
	}

	// New connection path.
	if sess == nil {
		token = uuid.New().String()
		sess = cm.sessions.createOrRefresh(opts.User, token)
		cm.logger.Printf("[chat] user %s connected (new session %s)", opts.User.UUID, token)
	}

	client := &Client{
		hub:    cm.hub,
		conn:   conn,
		send:   make(chan Message, sendBufferSize),
		user:   opts.User,
		token:  token,
		logger: cm.logger,
	}

	cm.hub.register(client)

	// Send the session token to the client immediately so it can persist it.
	sessionMsg := Message{
		ID:        "SYSTEM_SESSION",
		Type:      MessageTypeText,
		SenderID:  "SYSTEM",
		TargetID:  opts.User.UUID,
		Content:   token,
		Timestamp: time.Now().UTC(),
	}
	client.send <- sessionMsg

	// Replay missed messages for each requested conversation.
	for convID, isGroup := range opts.HistoryConversations {
		lastSeen := sess.getLastSeen(convID)
		if err := cm.hub.ReplayHistory(ctx, client, convID, isGroup, lastSeen); err != nil {
			cm.logger.Printf("[chat] replay error for %s/%s: %v", opts.User.UUID, convID, err)
		}
	}

	// Start pumps — each runs in its own goroutine (the Go way).
	go client.WritePump()
	go client.ReadPump(ctx)
}

// SendMessage allows the server to programmatically inject a message.
func (cm *ChatModule) SendMessage(ctx context.Context, msg Message) error {
	streamID, err := cm.hub.SaveMessage(ctx, msg)
	if err != nil {
		return err
	}
	msg.ID = streamID

	if msg.IsGroup {
		cm.hub.deliverToGroup(ctx, msg)
		return nil
	}
	return cm.hub.Publish(ctx, msg.TargetID, msg)
}

// ─────────────────────────────────────────────────────────────────────────────
// Group Management
// ─────────────────────────────────────────────────────────────────────────────

// CreateGroup creates a new group chat room.
func (cm *ChatModule) CreateGroup(ctx context.Context, g Group) error {
	if g.ID == "" {
		g.ID = uuid.New().String()
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	return cm.groups.CreateGroup(ctx, g)
}

// GetGroup fetches group metadata.
func (cm *ChatModule) GetGroup(ctx context.Context, groupID string) (*Group, error) {
	return cm.groups.GetGroup(ctx, groupID)
}

// AddGroupMember adds a user to an existing group.
func (cm *ChatModule) AddGroupMember(ctx context.Context, groupID, userUUID string) error {
	return cm.groups.AddMember(ctx, groupID, userUUID)
}

// RemoveGroupMember removes a user from a group.
func (cm *ChatModule) RemoveGroupMember(ctx context.Context, groupID, userUUID string) error {
	return cm.groups.RemoveMember(ctx, groupID, userUUID)
}

// ─────────────────────────────────────────────────────────────────────────────
// Chat List & History
// ─────────────────────────────────────────────────────────────────────────────

// GetChatList returns the most recent conversations for a user.
func (cm *ChatModule) GetChatList(ctx context.Context, userUUID string, limit int64) ([]ChatListEntry, error) {
	return cm.hub.GetChatList(ctx, userUUID, limit)
}

// GetHistory fetches paginated history for a conversation.
// lastID: Redis Stream ID to read from (exclusive). Use "0" for the oldest messages.
func (cm *ChatModule) GetHistory(ctx context.Context, userUUID, conversationID string, isGroup bool, lastID string, count int64) ([]Message, error) {
	streamKey := conversationStreamKey(userUUID, conversationID, isGroup)
	if isGroup {
		streamKey = fmt.Sprintf(streamKeyFmt, conversationID)
	}

	entries, err := cm.redis.XRead(ctx, &redis.XReadArgs{
		Streams: []string{streamKey, lastID},
		Count:   count,
	}).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	messages := make([]Message, 0, len(entries[0].Messages))
	for _, xmsg := range entries[0].Messages {
		payload, _ := xmsg.Values["payload"].(string)
		var m Message
		if err := json.Unmarshal([]byte(payload), &m); err == nil {
			messages = append(messages, m)
		}
	}
	return messages, nil
}

// MarkAsRead resets the unread counter for a conversation.
func (cm *ChatModule) MarkAsRead(ctx context.Context, userUUID, conversationID string) error {
	return cm.hub.MarkAsRead(ctx, userUUID, conversationID)
}

// ─────────────────────────────────────────────────────────────────────────────
// Presence
// ─────────────────────────────────────────────────────────────────────────────

// IsOnline checks if a user has an active WebSocket connection on this node.
func (cm *ChatModule) IsOnline(userUUID string) bool {
	return cm.hub.IsOnline(userUUID)
}

// OnlineUsers returns all user UUIDs connected to this node.
func (cm *ChatModule) OnlineUsers() []string {
	return cm.hub.OnlineUsers()
}

// Slog returns the structured logger so callers can use the same instance.
//
//	cm.Slog().Info("server started", "addr", ":8080")
func (cm *ChatModule) Slog() *slog.Logger {
	return cm.slog
}

// Close gracefully shuts down the chat module.
func (cm *ChatModule) Close() error {
	return cm.redis.Close()
}
