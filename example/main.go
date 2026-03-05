package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	chat "github.com/qtiso/chat_pubsub"
)

// ── Hardcoded test users (pre-seeded) ────────────────────────────────────────
// In production these would come from your auth/user service.
var testUsers = []chat.User{
	{UUID: "user-alice-001", Name: "Alice", PhotoURL: "https://i.pravatar.cc/150?u=alice"},
	{UUID: "user-bob-002", Name: "Bob", PhotoURL: "https://i.pravatar.cc/150?u=bob"},
}

func main() {
	// ── 1. Load config (.env → env vars → defaults) ───────────────────────────
	cfg, err := chat.LoadConfigFromEnv(".env")
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// ── 2. Initialise chat module ─────────────────────────────────────────────
	module, err := chat.New(cfg)
	if err != nil {
		slog.Error("chat.New failed", "error", err)
		os.Exit(1)
	}
	defer module.Close()

	log := module.Slog()

	// ── 3. Graceful shutdown ──────────────────────────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── 4. Start hub (Pub/Sub + session cleanup) ──────────────────────────────
	go module.Run(ctx)

	// ── 5. Seed demo group + 2 test users ────────────────────────────────────
	demoGroupID := "demo-group-001"
	module.CreateGroup(ctx, chat.Group{
		ID:        demoGroupID,
		Name:      "Demo Room",
		PhotoURL:  "https://i.pravatar.cc/150?u=demoroom",
		OwnerID:   "system",
		CreatedAt: time.Now().UTC(),
	})
	for _, u := range testUsers {
		module.AddGroupMember(ctx, demoGroupID, u.UUID)
	}

	if module.IsAPIKeyAuthEnabled() {
		log.Info("API key authentication enabled")
	} else {
		log.Warn("API key auth DISABLED — set CHAT_API_KEYS in production")
	}

	log.Info("test users ready",
		"alice_uuid", testUsers[0].UUID,
		"bob_uuid", testUsers[1].UUID,
		"group_id", demoGroupID,
	)

	// ─────────────────────────────────────────────────────────────────────────
	// ── 6. WebSocket server  (CHAT_WS_ADDR, default :8080) ───────────────────
	// ─────────────────────────────────────────────────────────────────────────
	//
	// Connect:
	//   ws://localhost:8080/ws?uuid=user-bob-002
	//   Header: X-API-Key: <key>   (or ?api_key=<key>)
	//
	// After connecting, send a JSON message:
	//   {
	//     "uuid":      "user-alice-001",   ← (optional if passed in URL)
	//     "name":      "Alice",            ← optional
	//     "type":      "text",
	//     "target_id": "user-bob-002",     ← PM recipient UUID
	//     "is_group":  false,
	//     "content":   "Hello Bob!"
	//   }

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}

	wsMux := http.NewServeMux()

	wsMux.Handle("/ws", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Error("ws upgrade failed", "error", err)
			return
		}
		// Identity can be provided via query params (e.g., ?uuid=bob-002)
		// OR via the first JSON message sent over the socket.
		module.Connect(conn, chat.ConnectOptions{
			User: chat.User{
				UUID:     r.URL.Query().Get("uuid"),
				Name:     r.URL.Query().Get("name"),
				PhotoURL: r.URL.Query().Get("photo"),
			},
			ReconnectToken: r.URL.Query().Get("token"),
		})
	}))

	// ── /ws/mux — Multiplexed gateway endpoint (API Server → Chat Server) ─────
	//
	// API Server maintains a POOL of N connections here.
	// Each connection carries messages for many users simultaneously.
	// Pool management (reconnect, rebalance) is the API Server's responsibility.
	//
	// Inbound frame:  { "user_id":"alice","user_name":"Alice","data":{...msg...} }
	// Outbound frame: { "user_id":"bob", "kind":"message", "data":{...msg...} }
	//
	// On disconnect: Chat Server unregisters all users on that connection.
	// API Server detects drop → reconnects → re-registers affected users.
	wsMux.Handle("/ws/mux", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Error("ws/mux upgrade failed", "error", err)
			return
		}
		log.Info("mux gateway connection established", "remote", r.RemoteAddr)
		module.ConnectMux(conn) // blocks until gateway disconnects
	}))

	wsServer := &http.Server{Addr: cfg.WSAddr, Handler: wsMux}
	go func() {
		log.Info("WebSocket server started", "addr", cfg.WSAddr)
		if err := wsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("ws server error", "error", err)
			os.Exit(1)
		}
	}()

	// ─────────────────────────────────────────────────────────────────────────
	// ── 7. REST API server  (CHAT_HTTP_ADDR, default :8081) ──────────────────
	// ─────────────────────────────────────────────────────────────────────────
	httpMux := http.NewServeMux()

	// GET /users  — list test users (handy for copy-pasting UUIDs)
	httpMux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"users":    testUsers,
			"group_id": demoGroupID,
			"hint":     "connect to ws://localhost:" + cfg.WSAddr[1:] + "/ws then send a message with uuid field",
		})
	})

	// GET /chat-list?uuid=<UUID>
	httpMux.Handle("/chat-list", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		userUUID := r.URL.Query().Get("uuid")
		if userUUID == "" {
			http.Error(w, `{"error":"missing uuid"}`, http.StatusBadRequest)
			return
		}
		list, err := module.GetChatList(r.Context(), userUUID, 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	}))

	// GET /history?uuid=<UUID>&conv=<ConvID>&group=true&last_id=<StreamID>
	httpMux.Handle("/history", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		msgs, err := module.GetHistory(r.Context(),
			q.Get("uuid"), q.Get("conv"), q.Get("group") == "true",
			func() string {
				if id := q.Get("last_id"); id != "" {
					return id
				}
				return "0"
			}(), 50)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(msgs)
	}))

	// GET /online
	httpMux.Handle("/online", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(module.OnlineUsers())
	}))

	// POST /group/member  body: {"group_id":"...","user_uuid":"..."}
	httpMux.Handle("/group/member", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			GroupID  string `json:"group_id"`
			UserUUID string `json:"user_uuid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := module.AddGroupMember(r.Context(), body.GroupID, body.UserUUID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: httpMux}
	go func() {
		log.Info("REST API server started", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("rest server error", "error", err)
			os.Exit(1)
		}
	}()

	// ── 8. Print quick-test cheatsheet ───────────────────────────────────────
	log.Info("──────────────────────────────────────────")
	log.Info("QUICK TEST — open 2 WebSocket connections:")
	log.Info("  Alice", "connect", "ws://localhost"+cfg.WSAddr+"/ws?uuid=user-alice-001")
	log.Info("  Bob  ", "connect", "ws://localhost"+cfg.WSAddr+"/ws?uuid=user-bob-002")
	log.Info("Send from Alice → Bob:")
	log.Info(`  {"type":"text","to":"user-bob-002","is_group":false,"content":"Hello Bob!"}`)
	log.Info("Send to group:")
	log.Info(`  {"uuid":"user-alice-001","type":"text","target_id":"demo-group-001","is_group":true,"content":"Hey group!"}`)
	log.Info("REST: GET users list", "url", "http://localhost"+cfg.HTTPAddr+"/users")
	log.Info("──────────────────────────────────────────")

	// ── 9. Wait for shutdown ──────────────────────────────────────────────────
	<-ctx.Done()
	log.Info("shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	wsServer.Shutdown(shutCtx)
	httpServer.Shutdown(shutCtx)
	log.Info("goodbye")
}
