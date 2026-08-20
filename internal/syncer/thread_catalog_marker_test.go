package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/discrawl/internal/store"
)

// errBotsOnly is the rejection a user token gets from the bot-only per-channel
// active-thread endpoint. It arrives for every channel, on every run.
func errBotsOnly() error {
	return errors.New(`HTTP 403 Forbidden, {"message": "Only bots can use this endpoint", "code": 20002}`)
}

func errUnknownChannel() error {
	return errors.New(`HTTP 404 Not Found, {"message": "Unknown Channel", "code": 10003}`)
}

func threadCatalogFixture(t *testing.T, client *fakeClient, channelType discordgo.ChannelType) (context.Context, *store.Store, *Syncer, map[string]*discordgo.Channel, *lockedBuffer) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "discrawl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	out := &lockedBuffer{}
	parent := &discordgo.Channel{ID: "c1", GuildID: "g1", Name: "general", Type: channelType}
	return ctx, s, New(client, s, newTestLogger(out)), map[string]*discordgo.Channel{"c1": parent}, out
}

// The production shape: under a user token the active listing is rejected with
// 20002 on every channel and the private archive with 50001, while the public
// archive answers. Neither rejection says the catalog is unreachable, so no
// marker is written and an existing one is cleared.
func TestThreadCatalogUserTokenRejectionsClearExistingMarker(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		threadErrors:          map[string]error{"c1": errBotsOnly()},
		archivedPrivateErrors: map[string]error{"c1": errMissingAccess()},
		publicArchived: map[string][]*discordgo.Channel{"c1": {
			{ID: "t1", GuildID: "g1", ParentID: "c1", Name: "archived", Type: discordgo.ChannelTypeGuildPublicThread},
		}},
	}
	ctx, s, svc, allChannels, out := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)
	// The 371 rows production is carrying today.
	require.NoError(t, s.SetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"), "missing_access"))

	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))

	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Empty(t, marker, "an existing marker must be cleared, not merely left unwritten")
	require.NotContains(t, out.String(), `msg="channel thread crawl skipped"`)
	require.Contains(t, allChannels, "t1")
}

func TestThreadCatalogPrivateArchiveRejectionWritesNoMarker(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		archivedPrivateErrors: map[string]error{"c1": errMissingAccess()},
		publicArchived:        map[string][]*discordgo.Channel{"c1": {{ID: "t1", GuildID: "g1", ParentID: "c1", Name: "archived", Type: discordgo.ChannelTypeGuildPublicThread}}},
	}
	ctx, s, svc, allChannels, _ := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)

	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))

	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Empty(t, marker)
}

func TestThreadCatalogMarkerWrittenWhenPublicArchiveFails(t *testing.T) {
	t.Parallel()

	// The public archive is required: losing it means part of the catalog is
	// genuinely unreachable, even though the active rejection is routine.
	client := &fakeClient{
		threadErrors:   map[string]error{"c1": errBotsOnly()},
		archivedErrors: map[string]error{"c1": errMissingAccess()},
	}
	ctx, s, svc, allChannels, out := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)

	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))

	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Equal(t, "missing_access", marker)
	require.Contains(t, out.String(), `level=WARN msg="channel thread crawl skipped"`)
}

func TestThreadCatalogMarkerWrittenWhenActiveFailsForAccessReason(t *testing.T) {
	t.Parallel()

	// A 403 Missing Access on the active listing is an access problem, not the
	// routine bot-only rejection, so it still counts.
	client := &fakeClient{threadErrors: map[string]error{"c1": errMissingAccess()}}
	ctx, s, svc, allChannels, _ := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)

	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))

	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Equal(t, "missing_access", marker)
}

func TestThreadCatalogReasonComesFromFirstFailingRequiredSource(t *testing.T) {
	t.Parallel()

	// Two required sources fail with different reasons. The active listing is
	// consulted first, so its reason is the one stored and it is not
	// overwritten by the archive answering later.
	client := &fakeClient{
		threadErrors:   map[string]error{"c1": errUnknownChannel()},
		archivedErrors: map[string]error{"c1": errMissingAccess()},
	}
	ctx, s, svc, allChannels, _ := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)

	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))

	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Equal(t, "unknown_channel", marker)
}

func TestThreadCatalogNonAccessFailureLeavesMarkerAlone(t *testing.T) {
	t.Parallel()

	client := &fakeClient{archivedErrors: map[string]error{"c1": errors.New("HTTP 503 Service Unavailable")}}
	ctx, s, svc, allChannels, out := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)

	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))

	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Empty(t, marker, "a transient failure must not be recorded as an access problem")
	require.Contains(t, out.String(), `msg="thread archive crawl failed"`)
}

func TestThreadCatalogRepeatedRejectionLogsOnce(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		threadErrors:   map[string]error{"c1": errMissingAccess()},
		archivedErrors: map[string]error{"c1": errMissingAccess()},
	}
	ctx, s, svc, allChannels, _ := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)
	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))

	second := &lockedBuffer{}
	svc = New(client, s, newTestLogger(second))
	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))
	require.NotContains(t, second.String(), `level=WARN msg="channel thread crawl skipped"`)
}

// The search fallback must keep firing under exactly the conditions it does
// today: the active listing was rejected, no threads were found for the parent,
// and the parent is a forum.
func TestThreadCatalogSearchFallbackStillFiresAfterMarkerChange(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		threadErrors: map[string]error{"c1": errBotsOnly()},
		searchThreads: map[string][]*discordgo.Channel{
			"c1": {{ID: "s1", GuildID: "g1", ParentID: "c1", Name: "found by search", Type: discordgo.ChannelTypeGuildPublicThread}},
		},
	}
	ctx, s, svc, allChannels, out := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildForum)

	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))

	require.Equal(t, 1, client.searchCalls["c1"], "the fallback must still fire")
	require.Contains(t, allChannels, "s1")
	require.Contains(t, out.String(), "falling back to search-based forum thread discovery")

	// The forum's only required source (the public archive) answered, so no
	// marker is recorded for a channel the fallback just rescued.
	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Empty(t, marker)
}

func TestThreadCatalogSearchFallbackSkippedWhenActiveSucceeds(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		activeThreads: map[string][]*discordgo.Channel{
			"c1": {{ID: "a1", GuildID: "g1", ParentID: "c1", Name: "active", Type: discordgo.ChannelTypeGuildPublicThread}},
		},
	}
	ctx, _, svc, allChannels, _ := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildForum)

	require.NoError(t, svc.appendThreadCatalog(ctx, allChannels, "g1", []string{"c1"}))
	require.Zero(t, client.searchCalls["c1"], "the fallback gate must not open when active answered")
}

func TestIncrementalArchivedCatalogIgnoresPrivateRejection(t *testing.T) {
	t.Parallel()

	client := &fakeClient{archivedPrivateErrors: map[string]error{"c1": errMissingAccess()}}
	ctx, s, svc, allChannels, _ := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)
	require.NoError(t, s.SetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"), "missing_access"))

	require.NoError(t, svc.appendIncrementalArchivedThreadCatalog(ctx, allChannels, []string{"c1"}))

	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Empty(t, marker)

	publicCursor, err := s.GetSyncState(ctx, channelArchivedThreadCursorScope("c1", false))
	require.NoError(t, err)
	parsed, err := time.Parse(time.RFC3339Nano, publicCursor)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().UTC(), parsed, time.Minute)
}

func TestIncrementalArchivedCatalogMarksWhenPublicFails(t *testing.T) {
	t.Parallel()

	client := &fakeClient{archivedErrors: map[string]error{"c1": errMissingAccess()}}
	ctx, s, svc, allChannels, _ := threadCatalogFixture(t, client, discordgo.ChannelTypeGuildText)

	require.NoError(t, svc.appendIncrementalArchivedThreadCatalog(ctx, allChannels, []string{"c1"}))

	marker, err := s.GetSyncState(ctx, channelThreadCatalogUnavailableScope("c1"))
	require.NoError(t, err)
	require.Equal(t, "missing_access", marker)
}
