package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	sessionTTL         = 1 * time.Hour
	sessionKeyPrefix   = "session:" // unused constant kept for clarity
	sessionCleanupTick = 10 * time.Minute
	sessionRedisKeyFmt = "chat:session:%s"   // key: reconnect token → JSON
	sessionUserKeyFmt  = "chat:user_sess:%s" // key: user UUID → token
)

// SessionInfo tracks a user's reconnection state.
type SessionInfo struct {
	UserUUID       string            `json:"user_uuid"`
	UserName       string            `json:"user_name"`
	UserPhoto      string            `json:"user_photo"`
	ReconnectToken string            `json:"reconnect_token"`
	LastSeenMsgIDs map[string]string `json:"last_seen_msg_ids"` // conversationID → last Redis Stream ID
	CreatedAt      time.Time         `json:"created_at"`
	LastSeenAt     time.Time         `json:"last_seen_at"`
}

// isExpired checks if the session has passed its TTL.
func (s *SessionInfo) isExpired() bool {
	return time.Since(s.LastSeenAt) > sessionTTL
}

// touch refreshes the session's last-seen timestamp.
func (s *SessionInfo) touch() {
	s.LastSeenAt = time.Now()
}

// updateLastSeen records the latest message ID seen in a conversation.
func (s *SessionInfo) updateLastSeen(conversationID, msgID string) {
	s.LastSeenMsgIDs[conversationID] = msgID
}

// getLastSeen returns the last Stream ID a user consumed for a conversation.
// Returns "0" if the conversation is new to this session (fetch from beginning).
func (s *SessionInfo) getLastSeen(conversationID string) string {
	if id, ok := s.LastSeenMsgIDs[conversationID]; ok && id != "" {
		return id
	}
	return "0"
}

// sessionManager handles session lifecycle.
// Sessions are stored both in-memory (for speed) and in Redis (for durability
// across server restarts). Redis is optional — if nil, only in-memory is used.
type sessionManager struct {
	mu       sync.Mutex
	sessions map[string]*SessionInfo // key: reconnect token
	byUser   map[string]string       // key: user UUID → token
	redis    *redis.Client           // nil = no persistence
}

func newSessionManager(rdb *redis.Client) *sessionManager {
	return &sessionManager{
		sessions: make(map[string]*SessionInfo),
		byUser:   make(map[string]string),
		redis:    rdb,
	}
}

// persist writes a session to Redis with a TTL.
func (sm *sessionManager) persist(ctx context.Context, sess *SessionInfo) {
	if sm.redis == nil {
		return
	}
	data, err := json.Marshal(sess)
	if err != nil {
		return
	}
	ttl := sessionTTL - time.Since(sess.LastSeenAt)
	if ttl <= 0 {
		return
	}
	pipe := sm.redis.Pipeline()
	pipe.SetEX(ctx, fmt.Sprintf(sessionRedisKeyFmt, sess.ReconnectToken), string(data), ttl)
	pipe.SetEX(ctx, fmt.Sprintf(sessionUserKeyFmt, sess.UserUUID), sess.ReconnectToken, ttl)
	pipe.Exec(ctx)
}

// loadFromRedis tries to restore a session from Redis by token.
func (sm *sessionManager) loadFromRedis(ctx context.Context, token string) (*SessionInfo, bool) {
	if sm.redis == nil {
		return nil, false
	}
	raw, err := sm.redis.Get(ctx, fmt.Sprintf(sessionRedisKeyFmt, token)).Result()
	if err != nil {
		return nil, false
	}
	var sess SessionInfo
	if err := json.Unmarshal([]byte(raw), &sess); err != nil {
		return nil, false
	}
	if sess.isExpired() {
		return nil, false
	}
	return &sess, true
}

// createOrRefresh either creates a new session or re-issues one for the same user.
func (sm *sessionManager) createOrRefresh(user User, token string) *SessionInfo {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// If this user already has a valid in-memory session, refresh it.
	if oldToken, ok := sm.byUser[user.UUID]; ok {
		if sess, ok := sm.sessions[oldToken]; ok && !sess.isExpired() {
			sess.touch()
			go sm.persist(context.Background(), sess)
			return sess
		}
		delete(sm.sessions, oldToken) // clean up expired
	}

	sess := &SessionInfo{
		UserUUID:       user.UUID,
		UserName:       user.Name,
		UserPhoto:      user.PhotoURL,
		ReconnectToken: token,
		LastSeenMsgIDs: make(map[string]string),
		CreatedAt:      time.Now(),
		LastSeenAt:     time.Now(),
	}
	sm.sessions[token] = sess
	sm.byUser[user.UUID] = token
	go sm.persist(context.Background(), sess)
	return sess
}

// get looks up a session by its reconnect token.
// Falls back to Redis if not found in memory (e.g. after a server restart).
func (sm *sessionManager) get(token string) (*SessionInfo, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Fast path: in-memory.
	if sess, ok := sm.sessions[token]; ok {
		if sess.isExpired() {
			delete(sm.byUser, sess.UserUUID)
			delete(sm.sessions, token)
			return nil, false
		}
		sess.touch()
		return sess, true
	}

	// Slow path: Redis (covers server restart).
	sess, ok := sm.loadFromRedis(context.Background(), token)
	if !ok {
		return nil, false
	}
	sess.touch()
	sm.sessions[token] = sess
	sm.byUser[sess.UserUUID] = token
	return sess, true
}

// getByUser looks up a session by user UUID.
func (sm *sessionManager) getByUser(userUUID string) (*SessionInfo, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	token, ok := sm.byUser[userUUID]
	if !ok {
		return nil, false
	}
	sess, ok := sm.sessions[token]
	if !ok || sess.isExpired() {
		delete(sm.byUser, userUUID)
		if sess != nil {
			delete(sm.sessions, token)
		}
		return nil, false
	}
	sess.touch()
	return sess, true
}

// invalidate removes a session on explicit logout or TTL expiry.
func (sm *sessionManager) invalidate(token string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sess, ok := sm.sessions[token]; ok {
		delete(sm.byUser, sess.UserUUID)
		delete(sm.sessions, token)
		// Delete from Redis asynchronously.
		if sm.redis != nil {
			go func() {
				ctx := context.Background()
				sm.redis.Del(ctx,
					fmt.Sprintf(sessionRedisKeyFmt, token),
					fmt.Sprintf(sessionUserKeyFmt, sess.UserUUID),
				)
			}()
		}
	}
}

// cleanup removes all expired sessions periodically.
func (sm *sessionManager) cleanup(ctx context.Context) {
	ticker := time.NewTicker(sessionCleanupTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.mu.Lock()
			for token, sess := range sm.sessions {
				if sess.isExpired() {
					delete(sm.byUser, sess.UserUUID)
					delete(sm.sessions, token)
				}
			}
			sm.mu.Unlock()
		}
	}
}
