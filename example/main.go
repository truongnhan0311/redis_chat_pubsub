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

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	chat "github.com/qtiso/chat_pubsub"
)

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

	// ── 5. Seed demo group ────────────────────────────────────────────────────
	demoGroupID := uuid.New().String()
	if err := module.CreateGroup(ctx, chat.Group{
		ID:        demoGroupID,
		Name:      "Demo Room",
		PhotoURL:  "https://example.com/demo.jpg",
		OwnerID:   "system",
		MemberIDs: []string{},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		log.Warn("could not create demo group", "error", err)
	}

	if module.IsAPIKeyAuthEnabled() {
		log.Info("API key authentication enabled")
	} else {
		log.Warn("API key auth is DISABLED — set CHAT_API_KEYS in production")
	}

	// ─────────────────────────────────────────────────────────────────────────
	// ── 6. WebSocket server  (CHAT_WS_ADDR, default :8080) ───────────────────
	// ─────────────────────────────────────────────────────────────────────────
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}

	wsMux := http.NewServeMux()

	// GET /ws?uuid=<UUID>&name=<Name>&photo=<URL>&token=<ReconnectToken>
	// Auth: X-API-Key: <key>  OR  ?api_key=<key>
	wsMux.Handle("/ws", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		userUUID := q.Get("uuid")
		if userUUID == "" {
			http.Error(w, `{"error":"missing uuid"}`, http.StatusBadRequest)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Error("ws upgrade failed", "error", err, "uuid", userUUID)
			return
		}

		user := chat.User{
			UUID:     userUUID,
			Name:     q.Get("name"),
			PhotoURL: q.Get("photo"),
		}

		// Auto-join demo group.
		if err := module.AddGroupMember(ctx, demoGroupID, userUUID); err != nil {
			log.Warn("add group member failed", "error", err)
		}

		log.Info("user connected", "uuid", userUUID, "name", user.Name,
			"reconnect", q.Get("token") != "")

		module.Connect(conn, chat.ConnectOptions{
			User:           user,
			ReconnectToken: q.Get("token"),
			HistoryConversations: map[string]bool{
				demoGroupID: true,
			},
		})
	}))

	wsServer := &http.Server{
		Addr:    cfg.WSAddr,
		Handler: wsMux,
	}

	go func() {
		log.Info("WebSocket server started", "addr", cfg.WSAddr)
		if err := wsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("WebSocket server error", "error", err)
			os.Exit(1)
		}
	}()

	// ─────────────────────────────────────────────────────────────────────────
	// ── 7. REST API server  (CHAT_HTTP_ADDR, default :8081) ──────────────────
	// ─────────────────────────────────────────────────────────────────────────
	httpMux := http.NewServeMux()

	// GET /chat-list?uuid=<UUID>
	httpMux.Handle("/chat-list", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		userUUID := r.URL.Query().Get("uuid")
		if userUUID == "" {
			http.Error(w, `{"error":"missing uuid"}`, http.StatusBadRequest)
			return
		}
		list, err := module.GetChatList(r.Context(), userUUID, 20)
		if err != nil {
			log.Error("get chat list failed", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	}))

	// GET /history?uuid=<UUID>&conv=<ConvID>&group=true&last_id=<StreamID>
	httpMux.Handle("/history", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		userUUID := q.Get("uuid")
		convID := q.Get("conv")
		isGroup := q.Get("group") == "true"
		lastID := q.Get("last_id")
		if lastID == "" {
			lastID = "0"
		}
		msgs, err := module.GetHistory(r.Context(), userUUID, convID, isGroup, lastID, 50)
		if err != nil {
			log.Error("get history failed", "error", err)
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
			log.Error("add group member failed", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpMux,
	}

	go func() {
		log.Info("REST API server started", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("REST server error", "error", err)
			os.Exit(1)
		}
	}()

	// ── 8. Wait for shutdown ──────────────────────────────────────────────────
	<-ctx.Done()
	log.Info("shutting down...")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()

	wsServer.Shutdown(shutCtx)
	httpServer.Shutdown(shutCtx)

	log.Info("goodbye")
}
