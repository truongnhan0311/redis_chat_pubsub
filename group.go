package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// groupManager handles group creation, membership, and metadata backed by Redis.
type groupManager struct {
	redis *redis.Client
}

func newGroupManager(rdb *redis.Client) *groupManager {
	return &groupManager{redis: rdb}
}

// CreateGroup creates a new group and stores its metadata and members in Redis.
func (gm *groupManager) CreateGroup(ctx context.Context, g Group) error {
	pipe := gm.redis.Pipeline()

	// Store group metadata.
	meta, _ := json.Marshal(g)
	pipe.HSet(ctx, fmt.Sprintf(groupMetaKeyFmt, g.ID), map[string]interface{}{
		"id":         g.ID,
		"name":       g.Name,
		"photo_url":  g.PhotoURL,
		"owner_id":   g.OwnerID,
		"created_at": g.CreatedAt.Format(time.RFC3339),
		"meta":       string(meta),
	})

	// Store member set.
	memberArgs := make([]interface{}, len(g.MemberIDs))
	for i, id := range g.MemberIDs {
		memberArgs[i] = id
	}
	pipe.SAdd(ctx, fmt.Sprintf(groupMembersKeyFmt, g.ID), memberArgs...)

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
func (gm *groupManager) getMembers(ctx context.Context, groupID string) ([]string, error) {
	members, err := gm.redis.SMembers(ctx, fmt.Sprintf(groupMembersKeyFmt, groupID)).Result()
	if err != nil {
		return nil, fmt.Errorf("smembers: %w", err)
	}
	return members, nil
}

// AddMember adds a user to a group.
func (gm *groupManager) AddMember(ctx context.Context, groupID, userUUID string) error {
	return gm.redis.SAdd(ctx, fmt.Sprintf(groupMembersKeyFmt, groupID), userUUID).Err()
}

// RemoveMember removes a user from a group.
func (gm *groupManager) RemoveMember(ctx context.Context, groupID, userUUID string) error {
	return gm.redis.SRem(ctx, fmt.Sprintf(groupMembersKeyFmt, groupID), userUUID).Err()
}

// IsMember checks if a user belongs to a group.
func (gm *groupManager) IsMember(ctx context.Context, groupID, userUUID string) (bool, error) {
	return gm.redis.SIsMember(ctx, fmt.Sprintf(groupMembersKeyFmt, groupID), userUUID).Result()
}
