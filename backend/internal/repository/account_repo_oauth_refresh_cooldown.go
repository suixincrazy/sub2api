package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Plain OAuth cooldown reasons are cached by GetTempUnschedStatus in this form.
// Compare the complete Redis value so that even another rule with the same TTL survives.
func (c *tempUnschedCache) DeleteOpenAIRefreshCooldownIfObserved(ctx context.Context, id int64, until time.Time, reason string) error {
	observed, err := json.Marshal(service.TempUnschedState{UntilUnix: until.Unix(), ErrorMessage: reason})
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s%d", tempUnschedPrefix, id)
	// The existing script compares ARGV[1] byte-for-byte before deleting KEYS[1].
	return leaderLockReleaseScript.Run(ctx, c.rdb, []string{key}, observed).Err()
}
