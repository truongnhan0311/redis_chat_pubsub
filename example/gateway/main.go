// Package main is an example API Server that:
//  1. Accepts client WebSocket connections (auth via JWT or any mechanism)
//  2. Maintains a pool of N multiplexed connections to the Chat Server
//  3. Routes client messages through the pool to the Chat Server
//  4. Delivers Chat Server responses back to the correct client
//
// Usage:
//
//	cd example/gateway && go run main.go
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
)

var logger = slog.Default()

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	chatServerURL := getEnv("CHAT_WS_MUX_URL", "ws://localhost:8080/ws/mux")
	chatAPIKey := getEnv("CHAT_API_KEY", "")
	listenAddr := getEnv("API_ADDR", ":9000")
	poolSize := 10 // CHAT_POOL_SIZE configurable in production

	// ── Build connection pool ─────────────────────────────────────────────────
	// All client → chat routing goes through this pool.
	// OnDeliver is called when Chat Server pushes a message for a user.
	clients := &clientRegistry{conns: make(map[string]*websocket.Conn)}

	pool := NewPool(ctx, PoolConfig{
		ChatServerURL: chatServerURL,
		APIKey:        chatAPIKey,
		Size:          poolSize,
		Logger:        logger,
		OnDeliver: func(userID, kind string, data json.RawMessage) {
			// Find the client's WS and forward.
			if conn := clients.get(userID); conn != nil {
				conn.WriteJSON(map[string]any{
					"kind": kind,
					"data": json.RawMessage(data),
				})
			}
		},
	})

	logger.Info("connection pool started",
		"chat_server", chatServerURL,
		"pool_size", poolSize,
	)

	// ── WebSocket endpoint for App Clients ────────────────────────────────────
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// 1. Authenticate client (JWT, cookie, etc.) — simplified here.
		userID := r.URL.Query().Get("uuid")
		userName := r.URL.Query().Get("name")
		userPhoto := r.URL.Query().Get("photo")
		if userID == "" {
			http.Error(w, `{"error":"missing uuid"}`, http.StatusBadRequest)
			return
		}

		// 2. Upgrade to WS.
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Error("upgrade failed", "err", err)
			return
		}
		defer conn.Close()

		// 3. Register client connection.
		clients.set(userID, conn)
		defer clients.remove(userID)

		logger.Info("client connected", "user_id", userID, "name", userName)

		// 4. Read loop — forward client messages to Chat Server via pool.
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				break
			}
			// Forward to Chat Server through the pool.
			// The pool picks the right WS slot and wraps in MuxInbound.
			if err := pool.Send(userID, userName, userPhoto, raw); err != nil {
				logger.Warn("pool send failed", "user_id", userID, "err", err)
			}
		}

		logger.Info("client disconnected", "user_id", userID)
	})

	logger.Info("API Server started", "addr", listenAddr)
	srv := &http.Server{Addr: listenAddr}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	srv.ListenAndServe()
}

// ─────────────────────────────────────────────────────────────────────────────
// clientRegistry — thread-safe map of userID → client WebSocket connection
// ─────────────────────────────────────────────────────────────────────────────

type clientRegistry struct {
	mu    sync.RWMutex
	conns map[string]*websocket.Conn
}

func (r *clientRegistry) set(userID string, conn *websocket.Conn) {
	r.mu.Lock()
	r.conns[userID] = conn
	r.mu.Unlock()
}

func (r *clientRegistry) get(userID string) *websocket.Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[userID]
}

func (r *clientRegistry) remove(userID string) {
	r.mu.Lock()
	delete(r.conns, userID)
	r.mu.Unlock()
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
