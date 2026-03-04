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
	// ── 1. Load config from .env (falls back to env vars / defaults) ─────────
	cfg, err := chat.LoadConfigFromEnv(".env")
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// ── 2. Initialise the chat module ────────────────────────────────────────
	module, err := chat.New(cfg)
	if err != nil {
		slog.Error("chat.New failed", "error", err)
		os.Exit(1)
	}
	defer module.Close()

	log := module.Slog() // Use the module's structured logger everywhere.

	// ── 3. Graceful shutdown via OS signal ───────────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── 4. Start background processing ───────────────────────────────────────
	go module.Run(ctx)

	// ── 5. Seed a demo group ─────────────────────────────────────────────────
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

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}

	// ── 6. HTTP routes ───────────────────────────────────────────────────────
	mux := http.NewServeMux()

	if module.IsAPIKeyAuthEnabled() {
		log.Info("API key authentication enabled")
	} else {
		log.Warn("API key auth is DISABLED — set CHAT_API_KEYS in production")
	}

	// GET /ws?uuid=<UUID>&name=<Name>&photo=<URL>&token=<ReconnectToken>
	// Header: X-API-Key: <key>  (or ?api_key=<key>)
	mux.Handle("/ws", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		userUUID := q.Get("uuid")
		if userUUID == "" {
			http.Error(w, "missing uuid", http.StatusBadRequest)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Error("websocket upgrade failed", "error", err, "uuid", userUUID)
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

		log.Info("user connected", "uuid", userUUID, "name", user.Name)

		module.Connect(conn, chat.ConnectOptions{
			User:           user,
			ReconnectToken: q.Get("token"),
			HistoryConversations: map[string]bool{
				demoGroupID: true,
			},
		})
	}))

	// GET /chat-list?uuid=<UUID>
	mux.Handle("/chat-list", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		userUUID := r.URL.Query().Get("uuid")
		if userUUID == "" {
			http.Error(w, "missing uuid", http.StatusBadRequest)
			return
		}
		list, err := module.GetChatList(r.Context(), userUUID, 20)
		if err != nil {
			log.Error("get chat list failed", "error", err, "uuid", userUUID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	}))

	// GET /history?uuid=<UUID>&conv=<ConvID>&group=true&last_id=<StreamID>
	mux.Handle("/history", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
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
	mux.Handle("/online", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(module.OnlineUsers())
	}))

	// POST /group/member  body: {"group_id":"...","user_uuid":"..."}
	mux.Handle("/group/member", module.APIKeyMiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			GroupID  string `json:"group_id"`
			UserUUID string `json:"user_uuid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := module.AddGroupMember(r.Context(), body.GroupID, body.UserUUID); err != nil {
			log.Error("add group member failed", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// ── 7. Start server ───────────────────────────────────────────────────────
	addr := os.Getenv("CHAT_SERVER_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		log.Info("server started", "addr", addr, "demo_group_id", demoGroupID)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()
	log.Info("shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown error", "error", err)
	}
	log.Info("goodbye")
}
