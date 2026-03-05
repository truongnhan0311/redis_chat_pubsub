# Chat Server — API Server Integration Guide

> Package: [`github.com/qtiso/chat_pubsub`](https://github.com/truongnhan0311/redis_chat_pubsub)

This document is for **API Server developers** who want to connect their service to the Chat Server using the multiplexed WebSocket pool (`/ws/mux`).

---

## Architecture

```
App Client ──WS──▶ API Server ──[Pool N WS]──▶ Chat Server
                   (auth/JWT)   conn_id / user  (/ws/mux endpoint)
                                per device      (Redis-backed)
```

- **API Server** authenticates users, manages the WS pool, routes messages.
- **Chat Server** routes, persists (Redis Streams), and broadcasts (Redis Pub/Sub).
- Each pool WS carries messages for **many users** simultaneously (multiplexed).

---

## Environment Variables (Chat Server)

| Variable | Default | Description |
|---|---|---|
| `CHAT_REDIS_ADDR` | `localhost:6379` | Redis address |
| `CHAT_REDIS_PASSWORD` | `""` | Redis password |
| `CHAT_REDIS_DB` | `0` | Redis DB index |
| `CHAT_WS_ADDR` | `:8080` | WebSocket server (clients + mux) |
| `CHAT_HTTP_ADDR` | `:8081` | REST API server |
| `CHAT_API_KEYS` | `""` (disabled) | Comma-separated API keys |
| `CHAT_LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `CHAT_LOG_FORMAT` | `text` | `text\|json` |

---

## Step 1 — Connect the pool

### Endpoint

```
ws://chat-server:8080/ws/mux
```

### Authentication

```
Header: X-API-Key: <your-api-key>
```

Set `CHAT_API_KEYS=key1,key2` on the Chat Server. The API Server must send one of these keys on every pool WS connection.

### Pool design

Maintain a **fixed pool of N WS connections** (recommended: 10). Each connection handles messages for many users simultaneously.

```
Pool slot 0 ──WS──▶ /ws/mux
Pool slot 1 ──WS──▶ /ws/mux
...
Pool slot 9 ──WS──▶ /ws/mux
```

**User → slot assignment** (consistent hash):
```go
import "hash/fnv"

func slotFor(userID string, poolSize int) int {
    h := fnv.New32a()
    h.Write([]byte(userID))
    return int(h.Sum32()) % poolSize
}
```

**Auto-reconnect** (keep pool at max at all times):
```go
for i := range pool {
    go func(idx int) {
        for {
            conn, err := websocket.Dial(chatServerURL, "X-API-Key: "+apiKey)
            if err != nil {
                time.Sleep(backoff) // exponential, max 30s
                continue
            }
            pool[idx] = conn
            reRegisterActiveUsers(idx, conn) // re-register users still on this slot
            runSlot(conn)                    // blocks until broken
            // broken → loop → reconnect
        }
    }(i)
}
```

> **Re-register rule:** on reconnect, only re-register users where `userSlot[uid] == slotIdx`.  
> Users that migrated away during downtime must NOT be pulled back.

---

## Step 2 — Wire Formats

### Inbound: API Server → Chat Server

Send this JSON frame for every message a client dispatches.

```json
{
  "conn_id":    "phone-abc123",
  "user_id":    "alice-001",
  "user_name":  "Alice",
  "user_photo": "https://cdn/alice.jpg",
  "data": {
    "type":      "text",
    "target_id": "bob-002",
    "is_group":  false,
    "content":   "Hello Bob!",
    "metadata":  {}
  }
}
```

| Field | Required | Description |
|---|---|---|
| `conn_id` | ✅ | **Unique per device connection** — phone ≠ laptop even for same user. If omitted, Chat Server uses `user_id` (single-device fallback). |
| `user_id` | ✅ | The authenticated user's UUID |
| `user_name` | ❌ | Display name (optional, synced on each send) |
| `user_photo` | ❌ | Avatar URL |
| `data.type` | ✅ | `text \| image \| pdf \| voice \| video` |
| `data.target_id` | ✅ | Recipient UUID (PM) or Group UUID |
| `data.is_group` | ❌ | `true` = group message, `target_id` is the group UUID |
| `data.content` | ✅ | Text body or media URL (S3/CDN) |
| `data.metadata` | ❌ | Media details (see Metadata section) |

---

### Outbound: Chat Server → API Server

```json
{
  "conn_id": "phone-abc123",
  "user_id": "bob-002",
  "kind":    "message",
  "data": {
    "id":           "1714000000456-0",
    "type":         "text",
    "sender_id":    "alice-001",
    "sender_name":  "Alice",
    "sender_photo": "https://cdn/alice.jpg",
    "target_id":    "bob-002",
    "is_group":     false,
    "content":      "Hello Bob!",
    "metadata":     {},
    "timestamp":    "2026-03-05T14:00:00Z"
  }
}
```

| Field | Description |
|---|---|
| `conn_id` | ⚠️ **Route to this device connection** — use this, not `user_id`, to find the correct client WS |
| `user_id` | Informational — which user owns this connection |
| `kind` | `"message"` = chat message / `"session"` = system frame |
| `data` | Full message payload |

> [!IMPORTANT]
> **Always route outbound by `conn_id`**, not `user_id`.  
> If Alice has phone + laptop, Chat Server sends two separate frames with different `conn_id`. Each frame goes to the correct device.

---

### Session Frame (`kind = "session"`)

Sent once after the first message identifies the device.

```json
{
  "conn_id": "phone-abc123",
  "user_id": "alice-001",
  "kind":    "session",
  "data": {
    "kind":    "session",
    "uuid":    "alice-001",
    "payload": "7bb4d539-f25b-454c-a0a0-dacbf166408e"
  }
}
```

Forward `payload` (the session token) to the client — client should persist it for reconnect.

---

## Step 3 — Handle Multi-Device

Same user on phone + laptop → two different `conn_id`:

```
Alice's Phone  ──conn_id="phone-abc"──▶ Chat Server
Alice's Laptop ──conn_id="laptop-xyz"──▶ Chat Server
```

When Bob sends Alice a message, Chat Server delivers to **both**:
```
MuxOutbound{conn_id:"phone-abc",  data:{...}} → slot 3 → API Server → phone WS
MuxOutbound{conn_id:"laptop-xyz", data:{...}} → slot 7 → API Server → laptop WS
```

**API Server must store `conn_id → clientWS` mapping:**

```go
// When client connects to API Server:
connID := uuid.New().String()           // unique per client WS
connToWS[connID] = clientWebSocket      // store mapping
userConns[userID] = append(userConns[userID], connID)

// When MuxOutbound arrives:
func onDeliver(connID, kind string, data json.RawMessage) {
    ws := connToWS[connID]
    if ws != nil {
        ws.WriteJSON(map[string]any{"kind": kind, "data": data})
    }
}

// When client disconnects:
delete(connToWS, connID)
// remove connID from userConns[userID]
```

---

## Message Types & Metadata

| `type` | `content` | Required `metadata` fields |
|---|---|---|
| `text` | Text body | *(none)* |
| `image` | CDN URL | `file_name`, `file_size`, `mime_type`, `width`, `height` |
| `pdf` | CDN URL | `file_name`, `file_size`, `mime_type`, `page_count` |
| `voice` | CDN URL | `file_name`, `file_size`, `mime_type`, `duration` |
| `video` | CDN URL | `file_name`, `file_size`, `mime_type`, `width`, `height`, `duration`, `thumbnail` |

```json
{
  "type":    "image",
  "content": "https://cdn.example.com/photo.jpg",
  "metadata": {
    "file_name": "photo.jpg",
    "file_size":  204800,
    "mime_type":  "image/jpeg",
    "width":      1280,
    "height":     720
  }
}
```

---

## Group Messages

```json
{
  "conn_id": "phone-abc123",
  "user_id": "alice-001",
  "data": {
    "type":      "text",
    "target_id": "<group-uuid>",
    "is_group":  true,
    "content":   "Hey everyone!"
  }
}
```

Outbound to each group member includes full group info (no extra lookup needed):

```json
{
  "data": {
    "group_id":    "<group-uuid>",
    "group_name":  "Team Chat",
    "group_photo": "https://cdn/group.jpg",
    "sender_id":   "alice-001",
    "content":     "Hey everyone!"
  }
}
```

---

## REST API (Chat Server — `CHAT_HTTP_ADDR`)

All endpoints require `X-API-Key` header (same key as WS pool).

| Method | Path | Query Params | Description |
|---|---|---|---|
| `GET` | `/chat-list` | `uuid=<userID>` | Conversation list for a user |
| `GET` | `/history` | `uuid=`, `conv=`, `group=true`, `last_id=` | Message history (paginated) |
| `GET` | `/online` | — | All online user UUIDs on this node |
| `POST` | `/group/member` | body: `{"group_id":"...","user_uuid":"..."}` | Add user to group |

---

## Complete Flow

```
Client Alice        API Server              Chat Server         Redis
     │                   │                       │                │
     │──connect WS───────▶│                      │                │
     │                   │──pool WS connect──────▶│               │
     │                   │  Header: X-API-Key     │               │
     │                   │                       │                │
     │──send message─────▶│                      │                │
     │                   │──MuxInbound───────────▶│               │
     │                   │  {conn_id, user_id,    │──XAdd─────────▶│
     │                   │   data:{type,target,   │  (Stream)      │
     │                   │   content}}            │──Publish───────▶│
     │                   │                       │  (Pub/Sub)     │
     │                   │                       │                │
     │                   │◀──MuxOutbound──────────│ (Bob's slot)  │
     │                   │  {conn_id:"bob-ph",    │                │
     │                   │   kind:"message",      │                │
     │                   │   data:{...}}          │                │
     │                   │                       │                │
     │  (Bob's WS)       │──forward to Bob───────▶               │
```

---

## Reference Implementation

See [`example/gateway/`](./example/gateway/) for a complete Go implementation:

- `pool.go` — connection pool with consistent hash, auto-reconnect, exponential backoff
- `main.go` — minimal API Server demo

```go
pool := NewPool(ctx, PoolConfig{
    ChatServerURL: "ws://localhost:8080/ws/mux",
    APIKey:        os.Getenv("CHAT_API_KEY"),
    Size:          10,
    OnDeliver: func(connID, kind string, data json.RawMessage) {
        if ws := connRegistry.Get(connID); ws != nil {
            ws.WriteJSON(map[string]any{"kind": kind, "data": data})
        }
    },
})

// Forward client message to Chat Server:
pool.Send(userID, userName, userPhoto, rawMessageJSON)
```
