package raindrop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	semconvai "github.com/traceloop/go-openllmetry/semconv-ai"
	traceloop "github.com/traceloop/go-openllmetry/traceloop-sdk"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type (
	Prompt       = traceloop.Prompt
	Message      = traceloop.Message
	Completion   = traceloop.Completion
	Usage        = traceloop.Usage
	ToolFunction = traceloop.ToolFunction
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

	// Extract the context from the workflow to ensure children span from it
	wfCtx := extractWorkflowContext(wf)
	if wfCtx == nil {
		wfCtx = ctx
	}

	return &WorkflowSpan{
		tracer:   t,
		workflow: wf,
		ctx:      wfCtx,
	}
}

// LogPrompt creates a standalone LLM span without requiring a workflow span.
func (t *Tracer) LogPrompt(ctx context.Context, prompt Prompt, cfg WorkflowSpanConfig) *LLMSpan {
	if t == nil || !t.Enabled() || strings.TrimSpace(cfg.Name) == "" {
		return newNoopLLMSpan(ctx)
	}
	// We need to adapt to v0.1.3 changes.
	mergedAssoc := mergeAssoc(t.associationProps, cfg.AssociationProperties)
	if mergedAssoc == nil {
		mergedAssoc = make(map[string]string)
	}
	// Set task_name association property for LLM spans
	mergedAssoc["task_name"] = "llm"
	ctxAttrs := traceloop.ContextAttributes{
		WorkflowName:          &cfg.Name,
		AssociationProperties: mergedAssoc,
	}

	span := t.client.LogPrompt(ctx, prompt, ctxAttrs)
	// LogPrompt now returns LLMSpan value, not pointer and error.
	return newLLMSpan(span, ctx)
}

type WorkflowSpanConfig struct {
	Name                  string
	AssociationProperties map[string]string
}

type TaskSpanConfig struct {
	Name string
}

type ToolSpanConfig struct {
	Name                  string
	Type                  string
	Function              ToolFunction
	AssociationProperties map[string]string
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

func (w *WorkflowSpan) LLMSpan(prompt Prompt) *LLMSpan {
	if w == nil || w.disabled || w.workflow == nil || w.tracer == nil || w.tracer.client == nil {
		return newNoopLLMSpan(w.Context())
	}

	// Use tracer's LogPrompt to pass association properties
	assoc := make(map[string]string)
	for k, v := range w.workflow.Attributes.AssociationProperties {
		assoc[k] = v
	}
	// Set task_name association property for LLM spans
	assoc["task_name"] = "llm"

	wfName := w.workflow.Attributes.Name
	ctxAttrs := traceloop.ContextAttributes{
		WorkflowName:          &wfName,
		AssociationProperties: assoc,
	}

	span := w.tracer.client.LogPrompt(w.Context(), prompt, ctxAttrs)
	return newLLMSpan(span, w.Context())
}

func (w *WorkflowSpan) NewTask(cfg TaskSpanConfig) *TaskSpan {
	if w == nil || w.disabled || w.workflow == nil || strings.TrimSpace(cfg.Name) == "" {
		return newNoopTask()
	}

	task := w.workflow.NewTask(cfg.Name)

	// Extract the context from the task
	taskCtx := extractTaskContext(task)
	if taskCtx == nil {
		// Fallback to parent context if extraction fails, though this means trace will be broken
		// but we should try to maintain at least something.
		taskCtx = w.Context()
	}

	return &TaskSpan{
		tracer: w.tracer,
		task:   task,
		ctx:    taskCtx,
	}
}

// NewTool creates a tool span manually using the underlying tracer, avoiding the need for an Agent.
func (w *WorkflowSpan) NewTool(cfg ToolSpanConfig) *ToolSpan {
	if w == nil || w.disabled || w.workflow == nil || strings.TrimSpace(cfg.Name) == "" {
		return newNoopTool()
	}

	tracer := extractTracer(w.tracer.client)
	if tracer == nil {
		return newNoopTool()
	}

	toolCtx, span := tracer.Start(w.ctx, fmt.Sprintf("%s.tool", cfg.Name))

	attrs := []attribute.KeyValue{
		semconvai.TraceloopWorkflowName.String(w.workflow.Attributes.Name),
		semconvai.TraceloopSpanKind.String(string(semconvai.SpanKindTool)),
		semconvai.TraceloopEntityName.String(cfg.Name),
	}

	// Add tool parameters if present
	if cfg.Function.Parameters != nil {
		if paramsJSON, err := json.Marshal(cfg.Function.Parameters); err == nil {
			attrs = append(attrs, attribute.String("traceloop.entity.input", string(paramsJSON)))
		}
	}

	// Add workflow association properties
	for k, v := range w.workflow.Attributes.AssociationProperties {
		attrs = append(attrs, attribute.String("traceloop.association.properties."+k, v))
	}

	// Add tool association properties
	for k, v := range cfg.AssociationProperties {
		attrs = append(attrs, attribute.String("traceloop.association.properties."+k, v))
	}

	// Set task_name association property for tool spans
	attrs = append(attrs, attribute.String("traceloop.association.properties.task_name", "tool"))

	span.SetAttributes(attrs...)

	return &ToolSpan{
		tracer:     w.tracer,
		workflow:   w,
		ctx:        toolCtx,
		properties: cfg.AssociationProperties,
	}
}

type TaskSpan struct {
	tracer   *Tracer
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

func (t *TaskSpan) LLMSpan(prompt Prompt) *LLMSpan {
	if t == nil || t.disabled || t.task == nil || t.tracer == nil || t.tracer.client == nil {
		return newNoopLLMSpan(t.Context())
	}

	// Use tracer's LogPrompt to pass association properties
	// We need to get workflow name from the task's parent workflow
	// For now, we'll use an empty workflow name since task doesn't expose workflow directly
	assoc := make(map[string]string)
	// Set task_name association property for LLM spans
	assoc["task_name"] = "llm"

	var wfName string
	ctxAttrs := traceloop.ContextAttributes{
		WorkflowName:          &wfName,
		AssociationProperties: assoc,
	}

	span := t.tracer.client.LogPrompt(t.Context(), prompt, ctxAttrs)
	return newLLMSpan(span, t.Context())
}

type ToolSpan struct {
	tracer     *Tracer
	workflow   *WorkflowSpan
	ctx        context.Context
	properties map[string]string
	disabled   bool
}

func newNoopTool() *ToolSpan {
	return &ToolSpan{disabled: true}
}

func (t *ToolSpan) End() {
	if t == nil || t.disabled || t.ctx == nil {
		return
	}
	trace.SpanFromContext(t.ctx).End()
}

// ReportResult logs the output of the tool execution.
func (t *ToolSpan) ReportResult(result string) {
	if t == nil || t.disabled || t.ctx == nil {
		return
	}
	span := trace.SpanFromContext(t.ctx)
	if span.IsRecording() {
		span.SetAttributes(attribute.String("traceloop.entity.output", result))
	}
}

// ReportError records an error that occurred during tool execution.
// It records the error on the span and sets the span status to error.
func (t *ToolSpan) ReportError(err error) {
	if t == nil || t.disabled || t.ctx == nil || err == nil {
		return
	}
	span := trace.SpanFromContext(t.ctx)
	if span.IsRecording() {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

func (t *ToolSpan) Context() context.Context {
	if t == nil || t.ctx == nil {
		return context.Background()
	}
	return t.ctx
}

func (t *ToolSpan) LLMSpan(prompt Prompt) *LLMSpan {
	if t == nil || t.disabled || t.tracer == nil || t.tracer.client == nil {
		return newNoopLLMSpan(t.Context())
	}

	// Manually call LogPrompt on the SDK client
	// Merge properties: Workflow + Tool
	assoc := make(map[string]string)
	if t.workflow != nil {
		for k, v := range t.workflow.workflow.Attributes.AssociationProperties {
			assoc[k] = v
		}
	}
	for k, v := range t.properties {
		assoc[k] = v
	}
	// Set task_name association property for LLM spans
	assoc["task_name"] = "llm"

	wfName := ""
	if t.workflow != nil {
		wfName = t.workflow.workflow.Attributes.Name
	}

	ctxAttrs := traceloop.ContextAttributes{
		WorkflowName:          &wfName,
		AssociationProperties: assoc,
	}

	span := t.tracer.client.LogPrompt(t.ctx, prompt, ctxAttrs)
	return newLLMSpan(span, t.ctx)
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
	l.once.Do(func() {
		l.span.LogCompletion(ctx, completion, usage)
	})
	return nil
}

func (l *LLMSpan) End(ctx context.Context) error {
	return l.RecordCompletion(ctx, Completion{}, Usage{})
}

// ReportError records an error that occurred during LLM invocation.
// It records the error on the span and sets the span status to error.
func (l *LLMSpan) ReportError(err error) {
	if l == nil || l.disabled || l.ctx == nil || err == nil {
		return
	}
	span := trace.SpanFromContext(l.ctx)
	if span.IsRecording() {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
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

// extractTracer uses reflection to get the tracerProvider and then a tracer
func extractTracer(tl *traceloop.Traceloop) trace.Tracer {
	if tl == nil {
		return nil
	}
	defer func() {
		_ = recover()
	}()

	val := reflect.ValueOf(tl).Elem()

	// Field: tracerProvider *trace.TracerProvider
	tpField := val.FieldByName("tracerProvider")
	if !tpField.IsValid() || tpField.IsNil() {
		return nil
	}

	// Unsafe access to pointer
	realTPField := reflect.NewAt(tpField.Type(), unsafe.Pointer(tpField.UnsafeAddr())).Elem()
	tp, ok := realTPField.Interface().(trace.TracerProvider)
	if !ok || tp == nil {
		return nil
	}

	// Field: config Config (to get TracerName)
	configField := val.FieldByName("config")
	tracerName := "traceloop.tracer"
	if configField.IsValid() {
		realConfigField := reflect.NewAt(configField.Type(), unsafe.Pointer(configField.UnsafeAddr())).Elem()
		cfg := realConfigField.Interface().(traceloop.Config)
		if cfg.TracerName != "" {
			tracerName = cfg.TracerName
		}
	}

	return tp.Tracer(tracerName)
}

// extractWorkflowContext retrieves the unexported context from a traceloop.Workflow.
func extractWorkflowContext(w *traceloop.Workflow) context.Context {
	if w == nil {
		return nil
	}
	defer func() {
		_ = recover()
	}()

	val := reflect.ValueOf(w).Elem()
	field := val.FieldByName("ctx")
	if !field.IsValid() {
		return nil
	}

	realField := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	return realField.Interface().(context.Context)
}

// extractTaskContext retrieves the unexported context from a traceloop.Task.
func extractTaskContext(t *traceloop.Task) context.Context {
	if t == nil {
		return nil
	}
	defer func() {
		_ = recover()
	}()

	val := reflect.ValueOf(t).Elem()
	field := val.FieldByName("ctx")
	if !field.IsValid() {
		return nil
	}

	realField := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	return realField.Interface().(context.Context)
}
