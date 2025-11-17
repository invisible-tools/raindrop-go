package raindrop

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestDeriveTracingBaseFromDefaultEndpoint(t *testing.T) {
	got := deriveTracingBase("https://api.raindrop.ai/v1/")
	want := "https://api.raindrop.ai/v1/traces"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestDeriveTracingBaseKeepsCustomPrefix(t *testing.T) {
	got := deriveTracingBase("https://example.com/custom/v1/")
	want := "https://example.com/custom/v1/traces"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestDeriveTracingBaseFallbackOnParseError(t *testing.T) {
	got := deriveTracingBase("not a url")
	want := "not a url/v1/traces"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestTracingEmitsLLMSpan(t *testing.T) {
	traceServer, traceRequests := newOTLPServer()
	defer traceServer.Close()

	eventServer, _ := newRecorderServer()
	defer eventServer.Close()

	client, err := NewClient(context.Background(), Config{
		WriteKey:             "test",
		Endpoint:             eventServer.URL + "/",
		HTTPClient:           eventServer.Client(),
		FlushInterval:        time.Hour,
		PartialFlushInterval: 10 * time.Millisecond,
		Tracing: TracingConfig{
			Enabled:     true,
			BaseURL:     traceServer.URL,
			ServiceName: "test-service",
		},
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	ctx := context.Background()
	interaction, err := client.Begin(ctx, BeginParams{
		Event:  "chat_test",
		UserID: "user-123",
		Input:  "Hello?",
		Model:  "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("Begin error: %v", err)
	}

	llmSpan, err := interaction.LLMSpan(ctx, LLMSpanConfig{
		Prompt: Prompt{
			Vendor: "openai",
			Mode:   "chat",
			Model:  "gpt-4o-mini",
			Messages: []Message{
				{Role: "user", Content: "Hello?"},
			},
		},
	})
	if err != nil {
		t.Fatalf("LLMSpan error: %v", err)
	}

	if err := llmSpan.RecordCompletion(ctx, Completion{
		Model: "gpt-4o-mini",
		Messages: []Message{
			{Role: "assistant", Content: "Hi!"},
		},
	}, Usage{}); err != nil {
		t.Fatalf("RecordCompletion error: %v", err)
	}
	if err := llmSpan.End(ctx); err != nil {
		t.Fatalf("End error: %v", err)
	}

	if err := interaction.Finish(ctx, FinishParams{Output: "Hi!"}); err != nil {
		t.Fatalf("Finish error: %v", err)
	}

	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}

	req := waitOTLPRequest(t, traceRequests)
	if req.path != "/v1/traces" {
		t.Fatalf("expected traces endpoint, got %s", req.path)
	}

	export := &collectortrace.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(req.body, export); err != nil {
		t.Fatalf("unmarshal OTLP payload: %v", err)
	}

	if !containsSpanWithAttr(export, "openai.chat", "llm.request.model", "gpt-4o-mini") {
		t.Fatalf("expected LLM span for openai.chat with model attribute")
	}

	if !containsSpanWithAttr(export, "chat_test.workflow", "traceloop.workflow.name", "chat_test") {
		t.Fatalf("expected workflow span with name chat_test")
	}

}

func TestTracerDisabledNoRequests(t *testing.T) {
	traceServer, traceRequests := newOTLPServer()
	defer traceServer.Close()

	eventServer, _ := newRecorderServer()
	defer eventServer.Close()

	client, err := NewClient(context.Background(), Config{
		WriteKey:             "test",
		Endpoint:             eventServer.URL + "/",
		HTTPClient:           eventServer.Client(),
		FlushInterval:        time.Hour,
		PartialFlushInterval: 10 * time.Millisecond,
		Tracing: TracingConfig{
			Enabled: false,
		},
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	ctx := context.Background()
	interaction, err := client.Begin(ctx, BeginParams{
		Event:  "chat_test",
		UserID: "user-123",
		Input:  "Hello?",
		Model:  "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("Begin error: %v", err)
	}

	llmSpan, err := interaction.LLMSpan(ctx, LLMSpanConfig{})
	if err != nil {
		t.Fatalf("LLMSpan error: %v", err)
	}
	if err := llmSpan.End(ctx); err != nil {
		t.Fatalf("End error: %v", err)
	}
	if err := interaction.Finish(ctx, FinishParams{}); err != nil {
		t.Fatalf("Finish error: %v", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}

	select {
	case req := <-traceRequests:
		t.Fatalf("expected no OTLP requests, got %s", req.path)
	default:
	}
}

func TestTaskSpanProducesChildSpan(t *testing.T) {
	traceServer, traceRequests := newOTLPServer()
	defer traceServer.Close()

	eventServer, _ := newRecorderServer()
	defer eventServer.Close()

	client, err := NewClient(context.Background(), Config{
		WriteKey:             "test",
		Endpoint:             eventServer.URL + "/",
		HTTPClient:           eventServer.Client(),
		FlushInterval:        time.Hour,
		PartialFlushInterval: 10 * time.Millisecond,
		Tracing: TracingConfig{
			Enabled:     true,
			BaseURL:     traceServer.URL,
			ServiceName: "test-service",
		},
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	ctx := context.Background()
	interaction, err := client.Begin(ctx, BeginParams{
		Event:  "workflow_test",
		UserID: "user-123",
		Input:  "Hello?",
		Model:  "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("Begin error: %v", err)
	}

	task := interaction.TaskSpan(ctx, TaskSpanConfig{Name: "prepare"})
	task.End()

	if err := interaction.Finish(ctx, FinishParams{}); err != nil {
		t.Fatalf("Finish error: %v", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}

	req := waitOTLPRequest(t, traceRequests)
	export := &collectortrace.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(req.body, export); err != nil {
		t.Fatalf("unmarshal OTLP payload: %v", err)
	}

	if !containsSpan(export, "workflow_test.workflow") {
		t.Fatalf("expected workflow span in export")
	}
	if !containsSpan(export, "prepare.task") {
		t.Fatalf("expected task span in export")
	}
}

type otlpRequest struct {
	path string
	body []byte
}

func newOTLPServer() (*httptest.Server, <-chan otlpRequest) {
	ch := make(chan otlpRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, _ := io.ReadAll(r.Body)
		ch <- otlpRequest{path: r.URL.Path, body: data}
		w.WriteHeader(http.StatusOK)
	}))
	return server, ch
}

func waitOTLPRequest(t *testing.T, ch <-chan otlpRequest) otlpRequest {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for OTLP request")
		return otlpRequest{}
	}
}

func containsSpanWithAttr(export *collectortrace.ExportTraceServiceRequest, spanName, attrKey, attrValue string) bool {
	for _, rs := range export.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if span.GetName() != spanName {
					continue
				}
				for _, attr := range span.GetAttributes() {
					if attr.GetKey() == attrKey && attr.GetValue().GetStringValue() == attrValue {
						return true
					}
				}
			}
		}
	}
	return false
}

func containsSpan(export *collectortrace.ExportTraceServiceRequest, name string) bool {
	for _, rs := range export.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if span.GetName() == name {
					return true
				}
			}
		}
	}
	return false
}
