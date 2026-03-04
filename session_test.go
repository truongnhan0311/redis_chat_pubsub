package chat

import (
	"testing"
	"time"
)

// ─── SessionInfo unit tests (no Redis needed) ────────────────────────────────

func TestSessionInfo_Expiry(t *testing.T) {
	user := User{UUID: "u1", Name: "Alice", PhotoURL: ""}
	sess := &SessionInfo{
		UserUUID:       user.UUID,
		ReconnectToken: "tok1",
		LastSeenMsgIDs: make(map[string]string),
		CreatedAt:      time.Now(),
		LastSeenAt:     time.Now(),
	}

	if sess.isExpired() {
		t.Error("fresh session should not be expired")
	}

	// Wind the clock back beyond TTL.
	sess.LastSeenAt = time.Now().Add(-(sessionTTL + time.Second))
	if !sess.isExpired() {
		t.Error("session should be expired after TTL")
	}
}

func TestSessionInfo_Touch(t *testing.T) {
	sess := &SessionInfo{
		LastSeenAt: time.Now().Add(-(sessionTTL + time.Second)),
	}
	if !sess.isExpired() {
		t.Fatal("expected expired before touch")
	}
	sess.touch()
	if sess.isExpired() {
		t.Error("session should NOT be expired after touch()")
	}
}

func TestSessionInfo_LastSeen(t *testing.T) {
	sess := &SessionInfo{LastSeenMsgIDs: make(map[string]string)}

	// Default for unknown conversation is "0".
	if got := sess.getLastSeen("conv-1"); got != "0" {
		t.Errorf("expected '0', got %q", got)
	}

	sess.updateLastSeen("conv-1", "1714000000000-0")
	if got := sess.getLastSeen("conv-1"); got != "1714000000000-0" {
		t.Errorf("expected stream ID, got %q", got)
	}
}

// ─── sessionManager unit tests (no Redis — rdb=nil) ──────────────────────────

func TestSessionManager_CreateAndGet(t *testing.T) {
	sm := newSessionManager(nil) // nil Redis → in-memory only
	user := User{UUID: "u1", Name: "Alice"}

	sess := sm.createOrRefresh(user, "tok-abc")
	if sess.UserUUID != "u1" {
		t.Errorf("got UUID %q, want u1", sess.UserUUID)
	}

	got, ok := sm.get("tok-abc")
	if !ok {
		t.Fatal("expected session to be found")
	}
	if got.UserUUID != "u1" {
		t.Errorf("got UUID %q, want u1", got.UserUUID)
	}
}

func TestSessionManager_RefreshExisting(t *testing.T) {
	sm := newSessionManager(nil)
	user := User{UUID: "u2", Name: "Bob"}

	first := sm.createOrRefresh(user, "tok-1")
	second := sm.createOrRefresh(user, "tok-2") // Should return same session.

	if first != second {
		t.Error("createOrRefresh should return the same session for the same user UUID")
	}
}

func TestSessionManager_Invalidate(t *testing.T) {
	sm := newSessionManager(nil)
	user := User{UUID: "u3", Name: "Carol"}
	sm.createOrRefresh(user, "tok-xyz")

	sm.invalidate("tok-xyz")

	if _, ok := sm.get("tok-xyz"); ok {
		t.Error("session should be gone after invalidate")
	}
}

func TestSessionManager_ExpiredSessionNotReturned(t *testing.T) {
	sm := newSessionManager(nil)
	user := User{UUID: "u4", Name: "Dave"}
	sess := sm.createOrRefresh(user, "tok-exp")

	// Expire it manually.
	sess.LastSeenAt = time.Now().Add(-(sessionTTL + time.Second))

	if _, ok := sm.get("tok-exp"); ok {
		t.Error("expired session should not be returned")
	}
}

func TestSessionManager_GetByUser(t *testing.T) {
	sm := newSessionManager(nil)
	user := User{UUID: "u5", Name: "Eve"}
	sm.createOrRefresh(user, "tok-eve")

	got, ok := sm.getByUser("u5")
	if !ok {
		t.Fatal("expected to find session by user UUID")
	}
	if got.UserUUID != "u5" {
		t.Errorf("got %q, want u5", got.UserUUID)
	}
}
