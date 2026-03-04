# chat_pubsub

**Production-grade distributed chat module** viết bằng Go. Sử dụng WebSocket cho client và Redis làm Pub/Sub broker + storage.

---

## Kiến trúc tổng thể

```
Client App
   │  WebSocket (full-duplex, persistent)
   ▼
App Server (Go)
├── Hub (Connection Manager)          ← quản lý clients online tại node này
│   ├── ReadPump goroutine/client     ← nhận tin từ client
│   └── WritePump goroutine/client    ← gửi tin xuống client
│
├── Session Manager                   ← token-based reconnect (1h TTL)
├── Group Manager                     ← Redis Sets cho group membership
│
├── Storage Layer (Redis Streams)     ← lưu message history, chat list, unread
│   ├── XAdd  → persist message
│   └── XRead → history replay
│
└── WAL (Write-Ahead Log)             ← on-disk durability, crash recovery
       │
       ▼
    Redis Pub/Sub (chat:pubsub:global)
       │
       ▼
  Other App Server nodes              ← scale ngang
```

---

## Cài đặt

```bash
go get github.com/qtiso/chat_pubsub
```

**Dependencies:**
- Redis ≥ 6.2 (supports Streams)
- Go ≥ 1.22

---

## Quick Start

```go
package main

import (
    "context"
    "net/http"
    "log"

    chat "github.com/qtiso/chat_pubsub"
    "github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func main() {
    ctx := context.Background()

    // 1. Khởi tạo module
    module, err := chat.New(chat.Config{
        RedisAddr: "localhost:6379",
    })
    if err != nil {
        log.Fatal(err)
    }

    // 2. Chạy background workers
    go module.Run(ctx)

    // 3. WebSocket handler
    http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
        conn, _ := upgrader.Upgrade(w, r, nil)

        user := chat.User{
            UUID:     r.URL.Query().Get("uuid"),
            Name:     r.URL.Query().Get("name"),
            PhotoURL: r.URL.Query().Get("photo"),
        }

        module.Connect(conn, chat.ConnectOptions{
            User:           user,
            ReconnectToken: r.URL.Query().Get("token"), // empty = new session
        })
    })

    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

---

## API Reference

### ChatModule Methods

| Method | Description |
|---|---|
| `New(cfg Config) (*ChatModule, error)` | Khởi tạo module, kiểm tra kết nối Redis |
| `Run(ctx)` | Bắt đầu Pub/Sub listener và session cleanup (chạy trong goroutine) |
| `Connect(conn, opts)` | Đăng ký WebSocket client, replay history nếu reconnect |
| `SendMessage(ctx, msg)` | Inject message từ server code |
| `CreateGroup(ctx, group)` | Tạo group chat mới |
| `AddGroupMember(ctx, groupID, userUUID)` | Thêm thành viên vào group |
| `RemoveGroupMember(ctx, groupID, userUUID)` | Xoá thành viên khỏi group |
| `GetChatList(ctx, userUUID, limit)` | Lấy danh sách chat của user (mới nhất trước) |
| `GetHistory(ctx, userUUID, convID, isGroup, lastID, count)` | Lấy lịch sử tin nhắn phân trang |
| `MarkAsRead(ctx, userUUID, convID)` | Reset unread counter |
| `IsOnline(userUUID)` | Kiểm tra user có đang kết nối không |
| `OnlineUsers()` | Danh sách user đang online tại node này |
| `Close()` | Đóng Redis connection |

---

## Message Format

### Gửi lên từ Client (JSON)

```json
{
  "type": "text",          // "text" | "image" | "pdf" | "voice" | "video"
  "target_id": "<uuid>",   // UUID người nhận hoặc Group ID
  "is_group": false,       // true nếu gửi vào group
  "content": "Hello!",     // text content hoặc URL file (S3/CDN)
  "metadata": {
    "file_name": "doc.pdf",
    "file_size": 102400,
    "mime_type": "application/pdf",
    "duration": 0
  }
}
```

### Nhận xuống từ Server (JSON)

```json
{
  "id": "1714567890123-0",    // Redis Stream ID
  "type": "text",
  "sender_id": "<uuid>",
  "sender_name": "Alice",
  "sender_photo": "https://...",
  "target_id": "<uuid>",
  "is_group": false,
  "content": "Hello!",
  "timestamp": "2024-05-01T12:00:00Z"
}
```

---

## Multimedia (Image, PDF, Voice, Video)

Không gửi binary qua WebSocket. Luồng đúng:

```
1. Client upload file → Storage (S3 / Cloudinary / GCS)
2. Nhận về public URL
3. Gửi tin nhắn: type="image", content="https://cdn.example.com/img.jpg"
```

Server chỉ xử lý metadata nhỏ → throughput cao.

---

## Session & Reconnect

Khi kết nối lần đầu, server trả về `SESSION_TOKEN` trong message đặc biệt. Client lưu token này (localStorage, file, secure storage).

Khi reconnect:
```
ws://server/ws?uuid=<UUID>&token=<RECONNECT_TOKEN>
```

Server sẽ tự động:
1. Khôi phục session (1 giờ TTL)
2. Replay tin nhắn đã bỏ lỡ

---

## WAL (Crash Recovery)

Để bảo vệ dữ liệu khi Redis restart:

```go
walModule, err := chat.WithWAL(module, "/var/data/chat.wal")
// Recover on startup
walModule.Recover(ctx)
// Then run normally
go module.Run(ctx)
```

---

## Scale

Chạy nhiều node App Server, cùng kết nối một Redis:

```
App Server 1 ──┐
App Server 2 ──┼── Redis Cluster (Pub/Sub + Streams)
App Server 3 ──┘
```

Redis Pub/Sub tự động đồng bộ tin nhắn giữa các node.

---

## Docker Compose (local dev)

```yaml
version: "3.9"
services:
  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    command: redis-server --appendonly yes

  chat:
    build: .
    ports:
      - "8080:8080"
    environment:
      - REDIS_ADDR=redis:6379
    depends_on:
      - redis
```

```bash
docker compose up -d
```

---

## Chạy example

```bash
cd example
go mod tidy
# Đảm bảo Redis đang chạy
go run main.go
```

Test với wscat:
```bash
npm install -g wscat
wscat -c "ws://localhost:8080/ws?uuid=user-1&name=Alice"
# Gõ (JSON):
{"type":"text","target_id":"user-2","is_group":false,"content":"Hello!"}
```
