package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// searchForumThreadsServer stands in for the guild message-search endpoint and
// records what SearchForumThreads actually requested.
type searchForumThreadsServer struct {
	calls   atomic.Int64
	method  string
	path    string
	rawPath string
	query   map[string][]string
}

func newSearchForumThreadsClient(t *testing.T, handler http.HandlerFunc) (*Client, *searchForumThreadsServer) {
	t.Helper()

	recorder := &searchForumThreadsServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v10/guilds/g1/messages/search", func(w http.ResponseWriter, r *http.Request) {
		recorder.calls.Add(1)
		recorder.method = r.Method
		recorder.path = r.URL.Path
		recorder.rawPath = r.URL.RequestURI()
		recorder.query = r.URL.Query()
		handler(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	// patchDiscordEndpoints rewrites package-level globals, so these tests
	// cannot run in parallel.
	t.Cleanup(patchDiscordEndpoints(server.URL + "/api/v10/"))

	client, err := New("token")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client, recorder
}

func TestSearchForumThreadsBuildsExpectedRequest(t *testing.T) {
	client, recorder := newSearchForumThreadsClient(t, writeJSON(map[string]any{
		"threads": []map[string]any{
			{"id": "t1", "guild_id": "g1", "parent_id": "c1", "name": "first thread", "type": 11},
			{"id": "t2", "guild_id": "g1", "parent_id": "c1", "name": "second thread", "type": 11},
		},
		"messages": []any{},
	}))

	threads, err := client.SearchForumThreads(context.Background(), "g1", "c1")
	require.NoError(t, err)

	require.Equal(t, int64(1), recorder.calls.Load())
	require.Equal(t, http.MethodGet, recorder.method)
	require.Equal(t, "/api/v10/guilds/g1/messages/search", recorder.path)
	require.Equal(t, "/api/v10/guilds/g1/messages/search?channel_id=c1&limit=25", recorder.rawPath)
	require.Equal(t, []string{"c1"}, recorder.query["channel_id"])
	require.Equal(t, []string{"25"}, recorder.query["limit"])

	require.Len(t, threads, 2)
	require.Equal(t, "t1", threads[0].ID)
	require.Equal(t, "first thread", threads[0].Name)
	require.Equal(t, "c1", threads[0].ParentID)
	require.Equal(t, "t2", threads[1].ID)
}

func TestSearchForumThreadsHandlesEmptyAndAbsentThreadList(t *testing.T) {
	client, _ := newSearchForumThreadsClient(t, writeJSON(map[string]any{"threads": []any{}}))
	threads, err := client.SearchForumThreads(context.Background(), "g1", "c1")
	require.NoError(t, err)
	require.Empty(t, threads)

	// A response with no threads key at all must not error.
	other, _ := newSearchForumThreadsClient(t, writeJSON(map[string]any{"messages": []any{}, "total_results": 0}))
	threads, err = other.SearchForumThreads(context.Background(), "g1", "c1")
	require.NoError(t, err)
	require.Empty(t, threads)
}

func TestSearchForumThreadsReturnsErrorOnForbidden(t *testing.T) {
	client, recorder := newSearchForumThreadsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Missing Access", "code": 50001}`))
	})

	threads, err := client.SearchForumThreads(context.Background(), "g1", "c1")
	require.Error(t, err)
	require.Nil(t, threads)
	require.ErrorContains(t, err, "search forum threads for channel c1")
	require.ErrorContains(t, err, "50001")
	require.Equal(t, int64(1), recorder.calls.Load(), "a 403 must not be retried")
}

// TestSearchForumThreadsRetriesWhileIndexWarms covers the HTTP 202 the guild
// message-search endpoint returns while Discord builds the guild's message
// index. The body carries a retry_after hint, and the forked discordgo honours
// it: the request is reissued after the advertised delay instead of surfacing
// an error that reads like a permission failure.
func TestSearchForumThreadsRetriesWhileIndexWarms(t *testing.T) {
	var attempts atomic.Int64
	client, recorder := newSearchForumThreadsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":              110000,
				"message":           "Index not yet available. Try again later.",
				"retry_after":       0.01,
				"documents_indexed": 0,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"threads": []map[string]any{
				{"id": "t1", "guild_id": "g1", "parent_id": "c1", "name": "warmed thread", "type": 11},
			},
			"messages": []any{},
		})
	})

	threads, err := client.SearchForumThreads(context.Background(), "g1", "c1")
	require.NoError(t, err, "a 202 with a retry_after hint is retried, not reported as a failure")
	require.Len(t, threads, 1)
	require.Equal(t, "t1", threads[0].ID)
	require.Equal(t, int64(2), recorder.calls.Load(), "the warming 202 is followed by exactly one retry")
}

// TestSearchForumThreadsGivesUpWhenIndexNeverWarms covers the other end of the
// same path: an endpoint stuck on 202 exhausts the retry budget rather than
// retrying forever, and the resulting error still names the status and the
// Discord error code so the caller can tell index warming from a permission
// failure.
func TestSearchForumThreadsGivesUpWhenIndexNeverWarms(t *testing.T) {
	client, recorder := newSearchForumThreadsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":              110000,
			"message":           "Index not yet available. Try again later.",
			"retry_after":       0.01,
			"documents_indexed": 0,
		})
	})
	client.session.MaxRestRetries = 2

	threads, err := client.SearchForumThreads(context.Background(), "g1", "c1")
	require.Error(t, err, "a 202 that never resolves still ends as an error")
	require.Nil(t, threads)
	require.ErrorContains(t, err, "search forum threads for channel c1")
	require.ErrorContains(t, err, "202")
	require.ErrorContains(t, err, "110000")
	// One initial attempt plus MaxRestRetries retries.
	require.Equal(t, int64(3), recorder.calls.Load(), "the retry budget bounds the number of attempts")
}

func TestSearchForumThreadsReturnsErrorOnMalformedBody(t *testing.T) {
	client, _ := newSearchForumThreadsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"threads": `))
	})

	threads, err := client.SearchForumThreads(context.Background(), "g1", "c1")
	require.Error(t, err)
	require.Nil(t, threads)
	require.ErrorContains(t, err, "unmarshal search results for channel c1")
}

func TestSearchForumThreadsHonoursContextCancellation(t *testing.T) {
	client, _ := newSearchForumThreadsClient(t, writeJSON(map[string]any{"threads": []any{}}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	threads, err := client.SearchForumThreads(ctx, "g1", "c1")
	require.Error(t, err)
	require.Nil(t, threads)
}
