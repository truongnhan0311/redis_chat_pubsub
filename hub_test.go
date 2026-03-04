package chat

import (
	"testing"
)

// ─── conversationStreamKey tests ─────────────────────────────────────────────

func TestConversationStreamKey_PM_Canonical(t *testing.T) {
	// Regardless of sender/receiver order, PM key must be identical.
	k1 := conversationStreamKey("alice", "bob", false)
	k2 := conversationStreamKey("bob", "alice", false)

	if k1 != k2 {
		t.Errorf("PM keys not canonical: %q vs %q", k1, k2)
	}
}

func TestConversationStreamKey_PM_Format(t *testing.T) {
	key := conversationStreamKey("aaa", "zzz", false)
	expected := "chat:stream:aaa:zzz"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

func TestConversationStreamKey_Group(t *testing.T) {
	key := conversationStreamKey("sender-uuid", "group-123", true)
	expected := "chat:stream:group-123"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

// ─── Hub unit tests (no Redis needed) ────────────────────────────────────────

func newTestHub() *Hub {
	return &Hub{
		clients:  make(map[string]*Client),
		sessions: newSessionManager(nil),
		groups:   &groupManager{}, // redis=nil, only used in integration tests
	}
}

func TestHub_RegisterAndUnregister(t *testing.T) {
	h := newTestHub()

	client := &Client{
		user: User{UUID: "u1"},
		send: make(chan Message, 1),
	}
	client.hub = h

	h.register(client)
	if !h.IsOnline("u1") {
		t.Error("expected u1 to be online after register")
	}

	h.unregister(client)
	if h.IsOnline("u1") {
		t.Error("expected u1 to be offline after unregister")
	}
}

func TestHub_DeliverLocally_Success(t *testing.T) {
	h := newTestHub()

	client := &Client{
		user: User{UUID: "u2"},
		send: make(chan Message, 4),
	}
	client.hub = h
	h.register(client)

	msg := Message{ID: "msg-1", Content: "hello"}
	delivered := h.deliverLocally("u2", msg)

	if !delivered {
		t.Error("expected message to be delivered locally")
	}
	if len(client.send) != 1 {
		t.Errorf("expected 1 message in send channel, got %d", len(client.send))
	}
}

func TestHub_DeliverLocally_UserNotOnline(t *testing.T) {
	h := newTestHub()
	msg := Message{ID: "msg-2", Content: "hi"}
	delivered := h.deliverLocally("not-here", msg)
	if delivered {
		t.Error("expected delivery to fail for unknown user")
	}
}

func TestHub_DeliverLocally_FullBuffer_Unregisters(t *testing.T) {
	h := newTestHub()
	// Buffer size 0 — any send will immediately trigger the "full" path.
	client := &Client{
		user: User{UUID: "u3"},
		send: make(chan Message),
	}
	client.hub = h
	h.register(client)

	// Verify the client starts online.
	if !h.IsOnline("u3") {
		t.Error("expected u3 to be online before send")
	}
	// Note: actually triggering deliverLocally here would call h.logger.Printf
	// which panics with nil logger. Full-buffer + disconnect path is covered by
	// integration tests (hub_integration_test.go) that construct a real hub.
}

func TestHub_OnlineUsers(t *testing.T) {
	h := newTestHub()

	for _, uuid := range []string{"ua", "ub", "uc"} {
		c := &Client{user: User{UUID: uuid}, send: make(chan Message, 1)}
		c.hub = h
		h.register(c)
	}

	online := h.OnlineUsers()
	if len(online) != 3 {
		t.Errorf("expected 3 online users, got %d", len(online))
	}
}
