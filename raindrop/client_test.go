package raindrop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type recordedRequest struct {
	path string
	body []byte
}

func newRecorderServer() (*httptest.Server, <-chan recordedRequest) {
	ch := make(chan recordedRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, _ := io.ReadAll(r.Body)
		ch <- recordedRequest{path: r.URL.Path, body: data}
		w.WriteHeader(http.StatusOK)
	}))
	return server, ch
}

func waitRequest(t *testing.T, ch <-chan recordedRequest) recordedRequest {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request")
		return recordedRequest{}
	}
}

type failingTransport struct {
	calls chan struct{}
}

func newFailingTransport() *failingTransport {
	return &failingTransport{
		calls: make(chan struct{}, 4),
	}
}

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	select {
	case f.calls <- struct{}{}:
	default:
	}
	return nil, errors.New("boom")
}

type blockingTransport struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (b *blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return nil, context.Canceled
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

func TestInteractionPartialLifecycle(t *testing.T) {
	server, requests := newRecorderServer()
	defer server.Close()

	traceServer, _ := newOTLPServer()
	defer traceServer.Close()

	client, err := NewClient(context.Background(), Config{
		WriteKey:             "test",
		Endpoint:             server.URL + "/",
		HTTPClient:           server.Client(),
		FlushInterval:        time.Hour, // avoid automatic flush interference
		PartialFlushInterval: 50 * time.Millisecond,
		Tracing: TracingConfig{
			Enabled:     true,
			BaseURL:     traceServer.URL,
			ServiceName: "test",
		},
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}
	defer client.Shutdown(context.Background())

	ctx := context.Background()
	interaction, err := client.Begin(ctx, BeginParams{
		Event:  "sample_chat",
		UserID: "user-1",
		Input:  "hello",
		Model:  "gpt-test",
	})
	if err != nil {
		t.Fatalf("Begin error: %v", err)
	}

	eventID := interaction.EventID()

	first := waitRequest(t, requests)
	if first.path != "/events/track_partial" {
		t.Fatalf("expected track_partial endpoint, got %s", first.path)
	}

	var firstPayload map[string]any
	if err := json.Unmarshal(first.body, &firstPayload); err != nil {
		t.Fatalf("unmarshal first payload: %v", err)
	}
	if got := firstPayload["event_id"]; got != eventID {
		t.Fatalf("expected event_id %s, got %v", eventID, got)
	}
	if pending, _ := firstPayload["is_pending"].(bool); !pending {
		t.Fatalf("expected first partial to be pending=true, got %v", firstPayload["is_pending"])
	}

	if err := interaction.Finish(ctx, FinishParams{
		Output: "response",
	}); err != nil {
		t.Fatalf("Finish error: %v", err)
	}

	second := waitRequest(t, requests)
	if second.path != "/events/track_partial" {
		t.Fatalf("expected track_partial endpoint, got %s", second.path)
	}

	var secondPayload map[string]any
	if err := json.Unmarshal(second.body, &secondPayload); err != nil {
		t.Fatalf("unmarshal second payload: %v", err)
	}
	if pending, _ := secondPayload["is_pending"].(bool); pending {
		t.Fatalf("expected final partial to be pending=false, got %v", pending)
	}

	if resumed := client.ResumeInteraction(eventID); resumed != nil {
		t.Fatalf("expected interaction to be removed after finish")
	}
}

func TestClientFlushSendsQueuedEndpoints(t *testing.T) {
	server, requests := newRecorderServer()
	defer server.Close()

	client, err := NewClient(context.Background(), Config{
		WriteKey:      "test",
		Endpoint:      server.URL + "/",
		HTTPClient:    server.Client(),
		FlushInterval: time.Hour,
		BatchSize:     1,
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}
	defer client.Shutdown(context.Background())

	ctx := context.Background()
	if err := client.TrackSignal(ctx, SignalParams{
		EventID: "evt-1",
		Name:    "thumbs_up",
	}); err != nil {
		t.Fatalf("TrackSignal error: %v", err)
	}

	if err := client.Identify(ctx, IdentifyParams{
		UserID: "user-123",
		Traits: map[string]any{"plan": "pro"},
	}); err != nil {
		t.Fatalf("Identify error: %v", err)
	}

	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush error: %v", err)
	}

	got := map[string]json.RawMessage{}
	for i := 0; i < 2; i++ {
		req := waitRequest(t, requests)
		got[req.path] = req.body
	}

	if _, ok := got["/signals/track"]; !ok {
		t.Fatalf("expected request to /signals/track, got %v", got)
	}
	if _, ok := got["/users/identify"]; !ok {
		t.Fatalf("expected request to /users/identify, got %v", got)
	}
}

func TestRetryDelayParsesHeader(t *testing.T) {
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Set("Retry-After", "120")
	if d := retryDelay(resp); d != 120*time.Second {
		t.Fatalf("expected 120s retry, got %s", d)
	}
	if d := retryDelay(nil); d != 500*time.Millisecond {
		t.Fatalf("expected default delay, got %s", d)
	}
}

func TestClientHealth(t *testing.T) {
	client := &Client{
		queue: make(chan envelope, 2),
	}
	client.queue <- envelope{}
	client.metrics.enqueued.Store(5)
	client.metrics.dropped.Store(1)
	client.metrics.sent.Store(4)
	client.metrics.failed.Store(2)

	health := client.Health()
	if health["queued"] != 1 {
		t.Fatalf("expected queued=1, got %v", health["queued"])
	}
	if health["enqueued"] != uint64(5) || health["dropped"] != uint64(1) {
		t.Fatalf("unexpected counters: %#v", health)
	}
}

func TestImmediateSendRecordsFailure(t *testing.T) {
	transport := newFailingTransport()
	client, err := NewClient(context.Background(), Config{
		WriteKey:             "test",
		Endpoint:             "https://example.com/",
		HTTPClient:           &http.Client{Transport: transport},
		FlushInterval:        time.Hour,
		PartialFlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}
	defer client.Shutdown(context.Background())

	ctx := context.Background()
	if _, err := client.Begin(ctx, BeginParams{
		Event:  "chat_test",
		UserID: "user-123",
	}); err != nil {
		t.Fatalf("Begin error: %v", err)
	}

	select {
	case <-transport.calls:
	case <-time.After(time.Second):
		t.Fatal("expected HTTP attempt for immediate payload")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if client.metrics.failed.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := client.metrics.failed.Load(); got == 0 {
		t.Fatalf("expected failed metric to increment after send failure, got %d", got)
	}
}

func TestShutdownContextTimesOutWhenHTTPBlocked(t *testing.T) {
	blocker := newBlockingTransport()
	client, err := NewClient(context.Background(), Config{
		WriteKey:             "test",
		Endpoint:             "https://example.com/",
		HTTPClient:           &http.Client{Transport: blocker},
		FlushInterval:        time.Hour,
		PartialFlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	ctx := context.Background()
	if _, err := client.Begin(ctx, BeginParams{
		Event:  "chat_test",
		UserID: "user-123",
	}); err != nil {
		t.Fatalf("Begin error: %v", err)
	}

	cleanupOnce := sync.Once{}
	cleanup := func() {
		cleanupOnce.Do(func() {
			close(blocker.release)
			_ = client.Shutdown(context.Background())
		})
	}
	defer cleanup()

	select {
	case <-blocker.started:
	case <-time.After(time.Second):
		t.Fatal("expected HTTP attempt to start")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("expected shutdown to complete when worker HTTP call is blocked, got %v", err)
	}
}
