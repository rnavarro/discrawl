package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIncompleteMessageChannelKindAllowlist pins the channel kinds that the
// backfill listing accepts. channelKind never emits "thread_news"; announcement
// threads are stored as "thread_announcement", which must stay listed.
func TestIncompleteMessageChannelKindAllowlist(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "discrawl.db"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	listed := []string{"text", "news", "announcement", "thread_public", "thread_private", "thread_announcement"}
	ignored := []string{"category", "forum", "voice", "dm", "type_13"}
	for _, kind := range append(append([]string{}, listed...), ignored...) {
		require.NoError(t, s.UpsertChannel(ctx, ChannelRecord{ID: kind, GuildID: "g1", Kind: kind, Name: kind, RawJSON: `{}`}))
	}

	ids, err := s.IncompleteMessageChannelIDs(ctx, "g1")
	require.NoError(t, err)
	require.ElementsMatch(t, listed, ids)

	all, err := s.IncompleteMessageChannelIDs(ctx, "")
	require.NoError(t, err)
	require.ElementsMatch(t, listed, all)
}
