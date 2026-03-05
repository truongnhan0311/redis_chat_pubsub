package chat

import (
	"log"
	"testing"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func newTestHub() *Hub {
	return &Hub{
		clients:     make(map[string][]*Client),
		gateways:    make(map[string]*muxGateway),
		muxConns:    make(map[string]*Client),
		muxGateways: make(map[string]*muxGateway),
		userConns:   make(map[string][]string),
		sessions:    newSessionManager(nil),
		groups:      &groupManager{},
		logger:      testLogger(),
	}
}

func makeClient(uuid string, bufSize int) *Client {
	return &Client{
		user: User{UUID: uuid},
		send: make(chan any, bufSize),
	}
}

func testLogger() *log.Logger {
	return log.New(log.Writer(), "[test] ", 0)
}

// ─── conversationStreamKey ────────────────────────────────────────────────────

func TestConversationStreamKey_PM_Canonical(t *testing.T) {
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

// ─── Direct WS: register / unregister ────────────────────────────────────────

func TestHub_RegisterAndUnregister(t *testing.T) {
	h := newTestHub()
	c := makeClient("u1", 1)
	c.hub = h

	h.register(c)
	if !h.IsOnline("u1") {
		t.Error("expected u1 to be online after register")
	}
	if len(h.clients["u1"]) != 1 {
		t.Errorf("expected 1 client, got %d", len(h.clients["u1"]))
	}

	h.unregister(c)
	if h.IsOnline("u1") {
		t.Error("expected u1 to be offline after unregister")
	}
}

// ─── Multi-device: same user on 2 direct WS clients ──────────────────────────

func TestHub_MultiDevice_BothReceiveMessage(t *testing.T) {
	h := newTestHub()

	phone := makeClient("alice", 4)
	phone.hub = h
	laptop := makeClient("alice", 4)
	laptop.hub = h

	h.register(phone)
	h.register(laptop)

	if len(h.clients["alice"]) != 2 {
		t.Fatalf("expected 2 clients for alice, got %d", len(h.clients["alice"]))
	}

	msg := Message{ID: "m1", Content: "hello"}
	delivered := h.deliverLocally("alice", msg)

	if !delivered {
		t.Error("expected delivery to succeed")
	}
	if len(phone.send) != 1 {
		t.Errorf("phone: expected 1 msg, got %d", len(phone.send))
	}
	if len(laptop.send) != 1 {
		t.Errorf("laptop: expected 1 msg, got %d", len(laptop.send))
	}
}

func TestHub_MultiDevice_UnregisterOne_OtherStillOnline(t *testing.T) {
	h := newTestHub()

	phone := makeClient("alice", 4)
	phone.hub = h
	laptop := makeClient("alice", 4)
	laptop.hub = h

	h.register(phone)
	h.register(laptop)
	h.unregister(phone)

	if !h.IsOnline("alice") {
		t.Error("alice should still be online (laptop connected)")
	}
	if len(h.clients["alice"]) != 1 {
		t.Errorf("expected 1 client left, got %d", len(h.clients["alice"]))
	}
}

func TestHub_MultiDevice_AllDisconnect_Offline(t *testing.T) {
	h := newTestHub()

	phone := makeClient("alice", 4)
	phone.hub = h
	laptop := makeClient("alice", 4)
	laptop.hub = h

	h.register(phone)
	h.register(laptop)
	h.unregister(phone)
	h.unregister(laptop)

	if h.IsOnline("alice") {
		t.Error("alice should be offline after all devices disconnect")
	}
}

// ─── DeliverLocally ───────────────────────────────────────────────────────────

func TestHub_DeliverLocally_Success(t *testing.T) {
	h := newTestHub()
	c := makeClient("u2", 4)
	c.hub = h
	h.register(c)

	if !h.deliverLocally("u2", Message{ID: "msg-1", Content: "hello"}) {
		t.Error("expected delivery to succeed")
	}
	if len(c.send) != 1 {
		t.Errorf("expected 1 msg, got %d", len(c.send))
	}
}

func TestHub_DeliverLocally_UserNotOnline(t *testing.T) {
	h := newTestHub()
	if h.deliverLocally("not-here", Message{}) {
		t.Error("expected delivery to fail for unknown user")
	}
}

func TestHub_DeliverLocally_FullBuffer_Unregisters(t *testing.T) {
	h := newTestHub()
	c := &Client{
		user:   User{UUID: "u3"},
		send:   make(chan any), // unbuffered — immediately full
		hub:    h,
		logger: testLogger(),
	}
	h.register(c)

	if !h.IsOnline("u3") {
		t.Error("expected u3 online before send")
	}
	h.deliverLocally("u3", Message{Content: "overflow"})
	if h.IsOnline("u3") {
		t.Error("u3 should be offline after buffer-full disconnect")
	}
}

// ─── OnlineUsers ──────────────────────────────────────────────────────────────

func TestHub_OnlineUsers(t *testing.T) {
	h := newTestHub()
	for _, uuid := range []string{"ua", "ub", "uc"} {
		c := makeClient(uuid, 1)
		c.hub = h
		h.register(c)
	}
	if got := len(h.OnlineUsers()); got != 3 {
		t.Errorf("expected 3 online users, got %d", got)
	}
}

// ─── Mux: registerMuxConn ─────────────────────────────────────────────────────

func newTestGateway(id string) *muxGateway {
	return &muxGateway{
		id:     id,
		conns:  make(map[string]bool),
		logger: testLogger(),
	}
}

func TestHub_MuxRegister_SingleDevice(t *testing.T) {
	h := newTestHub()
	gw := newTestGateway("gw-aabbccdd-1111-2222-3333-444444444444")
	h.gateways[gw.id] = gw

	h.registerMuxConn("conn-phone", "alice", "Alice", "", gw)

	if _, ok := h.muxConns["conn-phone"]; !ok {
		t.Error("expected conn-phone in muxConns")
	}
	if !h.IsOnline("alice") {
		t.Error("expected alice online via mux")
	}
	if len(h.userConns["alice"]) != 1 {
		t.Errorf("expected 1 device for alice, got %d", len(h.userConns["alice"]))
	}
}

func TestHub_MuxRegister_MultiDevice_SameUser(t *testing.T) {
	h := newTestHub()
	gw1 := newTestGateway("gw-00000001-0000-0000-0000-000000000001")
	gw2 := newTestGateway("gw-00000002-0000-0000-0000-000000000002")
	h.gateways[gw1.id] = gw1
	h.gateways[gw2.id] = gw2

	h.registerMuxConn("conn-phone", "alice", "Alice", "", gw1)
	h.registerMuxConn("conn-laptop", "alice", "Alice", "", gw2)

	if len(h.userConns["alice"]) != 2 {
		t.Errorf("expected 2 devices for alice, got %d", len(h.userConns["alice"]))
	}
	if _, ok := h.muxConns["conn-phone"]; !ok {
		t.Error("expected conn-phone registered")
	}
	if _, ok := h.muxConns["conn-laptop"]; !ok {
		t.Error("expected conn-laptop registered")
	}
}

func TestHub_MuxDeliver_FansOutToAllDevices(t *testing.T) {
	h := newTestHub()
	gw := newTestGateway("gw-00000003-0000-0000-0000-000000000003")
	h.gateways[gw.id] = gw

	h.registerMuxConn("conn-phone", "alice", "Alice", "", gw)
	h.registerMuxConn("conn-laptop", "alice", "Alice", "", gw)

	phone := h.muxConns["conn-phone"]
	laptop := h.muxConns["conn-laptop"]

	msg := Message{ID: "m1", Content: "hi"}
	if !h.deliverToMuxUser("alice", msg) {
		t.Error("expected delivery to succeed")
	}
	if len(phone.send) != 1 {
		t.Errorf("phone: expected 1 msg, got %d", len(phone.send))
	}
	if len(laptop.send) != 1 {
		t.Errorf("laptop: expected 1 msg, got %d", len(laptop.send))
	}
}

func TestHub_MuxGatewayDisconnect_RemovesAllConns(t *testing.T) {
	h := newTestHub()
	gw := newTestGateway("gw-00000004-0000-0000-0000-000000000004")
	h.gateways[gw.id] = gw

	h.registerMuxConn("conn-phone", "alice", "Alice", "", gw)
	h.registerMuxConn("conn-laptop", "alice", "Alice", "", gw)

	// Close send channels manually to stop drain goroutines.
	// (skip drain goroutines in unit test — no real WS conn)

	h.unregisterGateway(gw)

	if h.IsOnline("alice") {
		t.Error("alice should be offline after gateway disconnect")
	}
	if len(h.muxConns) != 0 {
		t.Errorf("muxConns should be empty, got %d entries", len(h.muxConns))
	}
	if len(h.userConns) != 0 {
		t.Errorf("userConns should be empty, got %d entries", len(h.userConns))
	}
}

func TestHub_MuxMigration_ConnMovesToNewGateway(t *testing.T) {
	h := newTestHub()
	gw1 := newTestGateway("gw-00000005-0000-0000-0000-000000000005")
	gw2 := newTestGateway("gw-00000006-0000-0000-0000-000000000006")
	h.gateways[gw1.id] = gw1
	h.gateways[gw2.id] = gw2

	// Register alice's phone on gw1.
	h.registerMuxConn("conn-phone", "alice", "Alice", "", gw1)
	if h.muxGateways["conn-phone"] != gw1 {
		t.Fatal("expected conn-phone on gw1")
	}

	// Re-register alice's phone on gw2 (failover/migration).
	h.registerMuxConn("conn-phone", "alice", "Alice", "", gw2)
	if h.muxGateways["conn-phone"] != gw2 {
		t.Error("expected conn-phone migrated to gw2")
	}
	// Alice's device count should still be 1 (same connID).
	if len(h.userConns["alice"]) != 1 {
		t.Errorf("expected 1 device for alice after migration, got %d", len(h.userConns["alice"]))
	}
}
