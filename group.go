package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	groupMemberCacheTTL    = 30 * time.Second
	groupMemberCacheKeyFmt = "chat:cache:members:%s" // Redis String, JSON []string, TTL 30s
)

// groupManager handles group creation, membership, and metadata backed by Redis.
// Member lists are cached in Redis itself (SETEX 30s) so all server nodes share
// the same consistent cache — no per-node in-memory maps needed.
type groupManager struct {
	redis *redis.Client
}

func newGroupManager(rdb *redis.Client) *groupManager {
	return &groupManager{redis: rdb}
}

// CreateGroup creates a new group and stores metadata + members in Redis.
func (gm *groupManager) CreateGroup(ctx context.Context, g Group) error {
	pipe := gm.redis.Pipeline()

	meta, _ := json.Marshal(g)
	pipe.HSet(ctx, fmt.Sprintf(groupMetaKeyFmt, g.ID), map[string]interface{}{
		"id":         g.ID,
		"name":       g.Name,
		"photo_url":  g.PhotoURL,
		"owner_id":   g.OwnerID,
		"created_at": g.CreatedAt.Format(time.RFC3339),
		"meta":       string(meta),
	})

	if len(g.MemberIDs) > 0 {
		memberArgs := make([]interface{}, len(g.MemberIDs))
		for i, id := range g.MemberIDs {
			memberArgs[i] = id
		}
		pipe.SAdd(ctx, fmt.Sprintf(groupMembersKeyFmt, g.ID), memberArgs...)
	}

	_, err := pipe.Exec(ctx)
	return err
}

// GetGroup returns group metadata.
func (gm *groupManager) GetGroup(ctx context.Context, groupID string) (*Group, error) {
	raw, err := gm.redis.HGet(ctx, fmt.Sprintf(groupMetaKeyFmt, groupID), "meta").Result()
	if err == redis.Nil {
		return nil, fmt.Errorf("group %s not found", groupID)
	}
	if err != nil {
		return nil, err
	}
	var g Group
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// getMembers returns all member UUIDs of a group.
// Flow:
//  1. Check Redis cache key  (chat:cache:members:<id>, TTL 30s)
//  2. Cache miss → read from authoritative Redis Set (chat:group:<id>:members)
//  3. Write result back to cache
//
// All server nodes share this cache — consistent across the cluster.
func (gm *groupManager) getMembers(ctx context.Context, groupID string) ([]string, error) {
	cacheKey := fmt.Sprintf(groupMemberCacheKeyFmt, groupID)

	// 1. Try Redis cache.
	cached, err := gm.redis.Get(ctx, cacheKey).Result()
	if err == nil {
		var members []string
		if jsonErr := json.Unmarshal([]byte(cached), &members); jsonErr == nil {
			return members, nil
		}
	}

	// 2. Cache miss: fetch from authoritative Set.
	members, err := gm.redis.SMembers(ctx, fmt.Sprintf(groupMembersKeyFmt, groupID)).Result()
	if err != nil {
		return nil, fmt.Errorf("smembers: %w", err)
	}

	// 3. Populate cache (fire-and-forget, non-blocking).
	if data, jsonErr := json.Marshal(members); jsonErr == nil {
		gm.redis.SetEX(ctx, cacheKey, string(data), groupMemberCacheTTL)
	}

	return members, nil
}

// invalidateCache deletes the Redis member cache for a group.
// Called after any membership change; the next getMembers will re-populate.
func (gm *groupManager) invalidateCache(ctx context.Context, groupID string) {
	gm.redis.Del(ctx, fmt.Sprintf(groupMemberCacheKeyFmt, groupID))
}

// AddMember adds a user to a group and invalidates the shared cache.
func (gm *groupManager) AddMember(ctx context.Context, groupID, userUUID string) error {
	if err := gm.redis.SAdd(ctx, fmt.Sprintf(groupMembersKeyFmt, groupID), userUUID).Err(); err != nil {
		return err
	}
	gm.invalidateCache(ctx, groupID)
	return nil
}

// RemoveMember removes a user from a group and invalidates the shared cache.
func (gm *groupManager) RemoveMember(ctx context.Context, groupID, userUUID string) error {
	if err := gm.redis.SRem(ctx, fmt.Sprintf(groupMembersKeyFmt, groupID), userUUID).Err(); err != nil {
		return err
	}
	gm.invalidateCache(ctx, groupID)
	return nil
}

// IsMember checks if a user belongs to a group.
func (gm *groupManager) IsMember(ctx context.Context, groupID, userUUID string) (bool, error) {
	return gm.redis.SIsMember(ctx, fmt.Sprintf(groupMembersKeyFmt, groupID), userUUID).Result()
}
