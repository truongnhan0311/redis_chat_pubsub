package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	streamMaxLen       = 1000                // Max messages kept per conversation stream
	replayBatchSize    = 50                  // Messages fetched per replay batch
	chatListKeyFmt     = "user:%s:chat_list" // Redis Sorted Set key for chat list
	streamKeyFmt       = "chat:stream:%s"    // Redis Stream key per conversation
	unreadKeyFmt       = "user:%s:unread:%s" // Redis counter for unread per conversation
	groupMembersKeyFmt = "group:%s:members"  // Redis Set for group members
	groupMetaKeyFmt    = "group:%s:meta"     // Redis Hash for group metadata
)

// conversationStreamKey returns the Redis Stream key for a conversation.
// For PM: canonical key is sorted pair of two UUIDs.
// For Group: key is the group ID.
func conversationStreamKey(senderID, targetID string, isGroup bool) string {
	if isGroup {
		return fmt.Sprintf(streamKeyFmt, targetID)
	}
	// Canonical PM key: always alphabetically sorted to avoid duplicates.
	if senderID < targetID {
		return fmt.Sprintf(streamKeyFmt, senderID+":"+targetID)
	}
	return fmt.Sprintf(streamKeyFmt, targetID+":"+senderID)
}

// SaveMessage persists a message to a Redis Stream and updates the chat lists
// of both sender and receiver (or all group members).
func (h *Hub) SaveMessage(ctx context.Context, m Message) (streamID string, err error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal message: %w", err)
	}

	streamKey := conversationStreamKey(m.SenderID, m.TargetID, m.IsGroup)

	streamID, err = h.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: streamMaxLen,
		Approx: true,
		Values: map[string]interface{}{"payload": string(data)},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("xadd: %w", err)
	}

	score := float64(time.Now().UnixMilli())

	// Update chat list for sender.
	h.redis.ZAdd(ctx, fmt.Sprintf(chatListKeyFmt, m.SenderID), &redis.Z{
		Score:  score,
		Member: m.TargetID,
	})

	if m.IsGroup {
		// Update chat list for all group members.
		members, _ := h.redis.SMembers(ctx, fmt.Sprintf(groupMembersKeyFmt, m.TargetID)).Result()
		pipe := h.redis.Pipeline()
		for _, memberID := range members {
			if memberID == m.SenderID {
				continue
			}
			pipe.ZAdd(ctx, fmt.Sprintf(chatListKeyFmt, memberID), &redis.Z{Score: score, Member: m.TargetID})
			pipe.Incr(ctx, fmt.Sprintf(unreadKeyFmt, memberID, m.TargetID))
		}
		pipe.Exec(ctx)
	} else {
		// Update chat list and unread count for recipient.
		h.redis.ZAdd(ctx, fmt.Sprintf(chatListKeyFmt, m.TargetID), &redis.Z{Score: score, Member: m.SenderID})
		h.redis.Incr(ctx, fmt.Sprintf(unreadKeyFmt, m.TargetID, m.SenderID))
	}

	return streamID, nil
}

// ReplayHistory fetches messages the user missed while offline and sends them to the client.
// lastID is the Redis Stream ID of the last message the user saw; use "0" for all history.
func (h *Hub) ReplayHistory(ctx context.Context, client *Client, conversationID string, isGroup bool, lastID string) error {
	streamKey := conversationStreamKey(client.user.UUID, conversationID, isGroup)
	if isGroup {
		streamKey = fmt.Sprintf(streamKeyFmt, conversationID)
	}

	entries, err := h.redis.XRead(ctx, &redis.XReadArgs{
		Streams: []string{streamKey, lastID},
		Count:   replayBatchSize,
		Block:   0,
	}).Result()
	if err == redis.Nil {
		return nil // No new messages.
	}
	if err != nil {
		return fmt.Errorf("xread: %w", err)
	}

	if len(entries) == 0 || len(entries[0].Messages) == 0 {
		return nil
	}

	for _, xmsg := range entries[0].Messages {
		payload, ok := xmsg.Values["payload"].(string)
		if !ok {
			continue
		}
		var m Message
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			continue
		}
		select {
		case client.send <- m:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Update the session's last-seen pointer for this conversation.
	lastXID := entries[0].Messages[len(entries[0].Messages)-1].ID
	if sess, ok := h.sessions.getByUser(client.user.UUID); ok {
		sess.updateLastSeen(conversationID, lastXID)
	}

	return nil
}

// GetChatList returns a user's conversations sorted by most recent activity.
func (h *Hub) GetChatList(ctx context.Context, userUUID string, limit int64) ([]ChatListEntry, error) {
	key := fmt.Sprintf(chatListKeyFmt, userUUID)
	convIDs, err := h.redis.ZRevRange(ctx, key, 0, limit-1).Result()
	if err != nil {
		return nil, fmt.Errorf("zrevrange: %w", err)
	}

	entries := make([]ChatListEntry, 0, len(convIDs))
	for _, convID := range convIDs {
		unreadKey := fmt.Sprintf(unreadKeyFmt, userUUID, convID)
		unread, _ := h.redis.Get(ctx, unreadKey).Int()

		entry := ChatListEntry{
			ConversationID: convID,
			UnreadCount:    unread,
		}

		// Try to get group metadata first.
		groupMeta, err := h.redis.HGetAll(ctx, fmt.Sprintf(groupMetaKeyFmt, convID)).Result()
		if err == nil && len(groupMeta) > 0 {
			entry.IsGroup = true
			entry.Name = groupMeta["name"]
			entry.PhotoURL = groupMeta["photo_url"]
		}

		entries = append(entries, entry)
	}
	return entries, nil
}

// MarkAsRead resets the unread counter for a user in a conversation.
func (h *Hub) MarkAsRead(ctx context.Context, userUUID, conversationID string) error {
	key := fmt.Sprintf(unreadKeyFmt, userUUID, conversationID)
	return h.redis.Del(ctx, key).Err()
}
