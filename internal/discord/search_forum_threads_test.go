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

// TestSearchForumThreadsTreats202AsError documents a real gap rather than a
// desired behavior. Discord answers the search endpoint with HTTP 202 while it
// builds the guild's message index, and expects the caller to retry after the
// advertised delay. The vendored discordgo accepts only 200, 201 and 204, so a
// 202 falls through to its default branch and becomes a REST error that reads
// exactly like a permission failure: the caller cannot tell "index warming,
// retry shortly" from "you may not read this channel", and the retry_after
// hint in the body is lost. Callers that need to distinguish the two must
// inspect the error text for the 202 status, because the vendored library is
// deliberately left unchanged here.
func TestSearchForumThreadsTreats202AsError(t *testing.T) {
	body := map[string]any{
		"code":              110000,
		"message":           "Index not yet available. Try again later.",
		"retry_after":       1.5,
		"documents_indexed": 0,
	}
	client, recorder := newSearchForumThreadsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(body)
	})

	threads, err := client.SearchForumThreads(context.Background(), "g1", "c1")
	require.Error(t, err, "202 is treated as a failure, not as a retryable success")
	require.Nil(t, threads)
	require.ErrorContains(t, err, "search forum threads for channel c1")
	// The status is the only thing separating this from a permission failure.
	require.ErrorContains(t, err, "202")
	require.ErrorContains(t, err, "110000")
	require.Equal(t, int64(1), recorder.calls.Load(), "202 is not retried by the REST layer")
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
