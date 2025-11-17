package raindrop

import (
	"context"
	"net/url"
	"path"
	"strings"
	"sync"

	traceloop "github.com/traceloop/go-openllmetry/traceloop-sdk"
)

type (
	Prompt     = traceloop.Prompt
	Message    = traceloop.Message
	Completion = traceloop.Completion
	Usage      = traceloop.Usage
)

// Tracer wraps the Traceloop Go SDK, providing ergonomic helpers while keeping
// tracing optional.
type Tracer struct {
	enabled          bool
	client           *traceloop.Traceloop
	associationProps map[string]string
}

func newTracer(ctx context.Context, cfg TracingConfig, ingestBase string) (*Tracer, error) {
	if !cfg.Enabled {
		return &Tracer{}, nil
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = deriveTracingBase(ingestBase)
	}

	tl, err := traceloop.NewClient(ctx, traceloop.Config{
		APIKey:      cfg.APIKey,
		BaseURL:     baseURL,
		ServiceName: cfg.ServiceName,
	})
	if err != nil {
		return nil, err
	}

	return &Tracer{
		enabled:          true,
		client:           tl,
		associationProps: cloneMap(cfg.AssociationProperties),
	}, nil
}

// Close shuts down the underlying exporter.
func (t *Tracer) Close(ctx context.Context) error {
	if t == nil || t.client == nil {
		return nil
	}
	t.client.Shutdown(ctx)
	return nil
}

// Enabled returns true when tracing is active.
func (t *Tracer) Enabled() bool {
	return t != nil && t.enabled && t.client != nil
}

// NewWorkflow creates a workflow span rooted at the provided context.
func (t *Tracer) NewWorkflow(ctx context.Context, cfg WorkflowSpanConfig) *WorkflowSpan {
	if t == nil || !t.Enabled() || strings.TrimSpace(cfg.Name) == "" {
		return newNoopWorkflow()
	}

	attrs := traceloop.WorkflowAttributes{
		Name:                  cfg.Name,
		AssociationProperties: mergeAssoc(t.associationProps, cfg.AssociationProperties),
	}
	wf := t.client.NewWorkflow(ctx, attrs)
	return &WorkflowSpan{
		tracer:   t,
		workflow: wf,
		ctx:      ctx,
	}
}

// LogPrompt creates a standalone LLM span without requiring a workflow span.
func (t *Tracer) LogPrompt(ctx context.Context, prompt Prompt, cfg WorkflowSpanConfig) (*LLMSpan, error) {
	if t == nil || !t.Enabled() || strings.TrimSpace(cfg.Name) == "" {
		return newNoopLLMSpan(ctx), nil
	}
	attrs := traceloop.WorkflowAttributes{
		Name:                  cfg.Name,
		AssociationProperties: mergeAssoc(t.associationProps, cfg.AssociationProperties),
	}
	span, err := t.client.LogPrompt(ctx, prompt, attrs)
	if err != nil {
		return nil, err
	}
	return newLLMSpan(span, ctx), nil
}

type WorkflowSpanConfig struct {
	Name                  string
	AssociationProperties map[string]string
}

type TaskSpanConfig struct {
	Name string
}

type WorkflowSpan struct {
	tracer   *Tracer
	workflow *traceloop.Workflow
	ctx      context.Context
	disabled bool
}

func newNoopWorkflow() *WorkflowSpan {
	return &WorkflowSpan{disabled: true}
}

func (w *WorkflowSpan) End() {
	if w == nil || w.disabled || w.workflow == nil {
		return
	}
	w.workflow.End()
}

func (w *WorkflowSpan) Context() context.Context {
	if w == nil || w.ctx == nil {
		return context.Background()
	}
	return w.ctx
}

func (w *WorkflowSpan) LLMSpan(prompt Prompt) (*LLMSpan, error) {
	if w == nil || w.disabled || w.workflow == nil {
		return newNoopLLMSpan(w.Context()), nil
	}
	span, err := w.workflow.LogPrompt(prompt)
	if err != nil {
		return nil, err
	}
	return newLLMSpan(span, w.Context()), nil
}

func (w *WorkflowSpan) NewTask(cfg TaskSpanConfig) *TaskSpan {
	if w == nil || w.disabled || w.workflow == nil || strings.TrimSpace(cfg.Name) == "" {
		return newNoopTask()
	}

	task := w.workflow.NewTask(cfg.Name)
	return &TaskSpan{
		task: task,
		ctx:  w.Context(),
	}
}

type TaskSpan struct {
	task     *traceloop.Task
	ctx      context.Context
	disabled bool
}

func newNoopTask() *TaskSpan {
	return &TaskSpan{disabled: true}
}

func (t *TaskSpan) End() {
	if t == nil || t.disabled || t.task == nil {
		return
	}
	t.task.End()
}

func (t *TaskSpan) Context() context.Context {
	if t == nil || t.ctx == nil {
		return context.Background()
	}
	return t.ctx
}

func (t *TaskSpan) LLMSpan(prompt Prompt) (*LLMSpan, error) {
	if t == nil || t.disabled || t.task == nil {
		return newNoopLLMSpan(t.Context()), nil
	}
	span, err := t.task.LogPrompt(prompt)
	if err != nil {
		return nil, err
	}
	return newLLMSpan(span, t.Context()), nil
}

type LLMSpan struct {
	span     traceloop.LLMSpan
	ctx      context.Context
	disabled bool
	once     sync.Once
}

func newLLMSpan(span traceloop.LLMSpan, ctx context.Context) *LLMSpan {
	if ctx == nil {
		ctx = context.Background()
	}
	return &LLMSpan{
		span: span,
		ctx:  ctx,
	}
}

func newNoopLLMSpan(ctx context.Context) *LLMSpan {
	if ctx == nil {
		ctx = context.Background()
	}
	return &LLMSpan{
		ctx:      ctx,
		disabled: true,
	}
}

func (l *LLMSpan) Context() context.Context {
	if l == nil || l.ctx == nil {
		return context.Background()
	}
	return l.ctx
}

func (l *LLMSpan) RecordCompletion(ctx context.Context, completion Completion, usage Usage) error {
	if l == nil || l.disabled {
		return nil
	}
	if ctx == nil {
		ctx = l.Context()
	}
	var err error
	l.once.Do(func() {
		err = l.span.LogCompletion(ctx, completion, usage)
	})
	return err
}

// End closes the span even if no completion data was recorded.
func (l *LLMSpan) End(ctx context.Context) error {
	return l.RecordCompletion(ctx, Completion{}, Usage{})
}

func mergeAssoc(global map[string]string, local map[string]string) map[string]string {
	if len(global) == 0 && len(local) == 0 {
		return nil
	}
	out := make(map[string]string, len(global)+len(local))
	for k, v := range global {
		out[k] = v
	}
	for k, v := range local {
		out[k] = v
	}
	return out
}

func cloneMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func deriveTracingBase(ingest string) string {
	parsed, err := url.Parse(ingest)
	if err != nil || parsed.Host == "" {
		base := strings.TrimSuffix(ingest, "/")
		if strings.HasSuffix(base, "/v1") {
			return base + "/traces"
		}
		return base + "/v1/traces"
	}
	basePath := strings.TrimSuffix(parsed.Path, "/")
	if basePath == "" {
		basePath = "/v1"
	}
	parsed.Path = path.Join(basePath, "traces")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
