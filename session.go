package chat

import (
	"context"
	"time"
)

const (
	sessionTTL         = 1 * time.Hour
	sessionKeyPrefix   = "session:"
	sessionCleanupTick = 10 * time.Minute
)

// SessionInfo tracks a user's reconnection state.
type SessionInfo struct {
	UserUUID       string            `json:"user_uuid"`
	UserName       string            `json:"user_name"`
	UserPhoto      string            `json:"user_photo"`
	ReconnectToken string            `json:"reconnect_token"`
	LastSeenMsgIDs map[string]string `json:"last_seen_msg_ids"` // conversationID -> last Redis Stream ID
	CreatedAt      time.Time         `json:"created_at"`
	LastSeenAt     time.Time         `json:"last_seen_at"`
}

// newSession creates a fresh session for a user.
func newSession(user User, token string) *SessionInfo {
	return &SessionInfo{
		UserUUID:       user.UUID,
		UserName:       user.Name,
		UserPhoto:      user.PhotoURL,
		ReconnectToken: token,
		LastSeenMsgIDs: make(map[string]string),
		CreatedAt:      time.Now(),
		LastSeenAt:     time.Now(),
	}
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

// sessionManager handles in-memory session lifecycle.
type sessionManager struct {
	sessions map[string]*SessionInfo // key: reconnect token
	byUser   map[string]string       // key: user UUID → token
	mu       chan struct{}           // mutex via channel trick for simplicity
}

func newSessionManager() *sessionManager {
	sm := &sessionManager{
		sessions: make(map[string]*SessionInfo),
		byUser:   make(map[string]string),
		mu:       make(chan struct{}, 1),
	}
	sm.mu <- struct{}{}
	return sm
}

func (sm *sessionManager) lock()   { <-sm.mu }
func (sm *sessionManager) unlock() { sm.mu <- struct{}{} }

// createOrRefresh either creates a new session or re-issues one for the same user.
func (sm *sessionManager) createOrRefresh(user User, token string) *SessionInfo {
	sm.lock()
	defer sm.unlock()

	// If this user already has a session, refresh it.
	if oldToken, ok := sm.byUser[user.UUID]; ok {
		if sess, ok := sm.sessions[oldToken]; ok && !sess.isExpired() {
			sess.touch()
			return sess
		}
		// Expired — clean up old token.
		delete(sm.sessions, oldToken)
	}

	sess := newSession(user, token)
	sm.sessions[token] = sess
	sm.byUser[user.UUID] = token
	return sess
}

// get looks up a session by its reconnect token.
func (sm *sessionManager) get(token string) (*SessionInfo, bool) {
	sm.lock()
	defer sm.unlock()
	sess, ok := sm.sessions[token]
	if !ok || sess.isExpired() {
		return nil, false
	}
	sess.touch()
	return sess, true
}

// getByUser looks up a session by user UUID.
func (sm *sessionManager) getByUser(userUUID string) (*SessionInfo, bool) {
	sm.lock()
	defer sm.unlock()
	token, ok := sm.byUser[userUUID]
	if !ok {
		return nil, false
	}
	sess, ok := sm.sessions[token]
	if !ok || sess.isExpired() {
		return nil, false
	}
	sess.touch()
	return sess, true
}

// invalidate removes a session on explicit logout or TTL expiry.
func (sm *sessionManager) invalidate(token string) {
	sm.lock()
	defer sm.unlock()
	if sess, ok := sm.sessions[token]; ok {
		delete(sm.byUser, sess.UserUUID)
		delete(sm.sessions, token)
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
			sm.lock()
			for token, sess := range sm.sessions {
				if sess.isExpired() {
					delete(sm.byUser, sess.UserUUID)
					delete(sm.sessions, token)
				}
			}
			sm.unlock()
		}
	}
}
