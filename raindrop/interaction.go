package raindrop

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Interaction wraps an in-flight AI event with helpers for partial updates.
type Interaction struct {
	client   *Client
	mu       sync.Mutex
	state    *trackRequest
	timer    *time.Timer
	interval time.Duration
	finished bool

	workflow *WorkflowSpan
}

func newInteraction(ctx context.Context, client *Client, state *trackRequest, interval time.Duration) *Interaction {
	i := &Interaction{
		client:   client,
		state:    state,
		interval: interval,
	}

	tracer := client.Tracer()
	if tracer.Enabled() {
		assoc := map[string]string{
			"event_id": state.EventID,
			"user_id":  state.UserID,
		}
		name := state.Event
		if name == "" {
			name = "interaction"
		}
		i.workflow = tracer.NewWorkflow(ctx, WorkflowSpanConfig{
			Name:                  name,
			AssociationProperties: assoc,
		})
	} else {
		i.workflow = newNoopWorkflow()
	}

	return i
}

// EventID exposes the server-correlated identifier.
func (i *Interaction) EventID() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.state.EventID
}

// SetProperty updates a single property and schedules a partial flush.
func (i *Interaction) SetProperty(ctx context.Context, key string, value any) error {
	data, err := normalizeProps(Props{key: value})
	if err != nil {
		return err
	}

	// Sync to trace attributes if workflow is active
	if i.workflow != nil && !i.workflow.disabled {
		if span := trace.SpanFromContext(i.workflow.Context()); span.IsRecording() {
			strVal := fmt.Sprintf("%v", value)
			span.SetAttributes(attribute.String("traceloop.association.properties."+key, strVal))
		}
	}

	return i.updateState(ctx, func(state *trackRequest) error {
		state.Properties = state.Properties.merge(data)
		return nil
	})
}

// SetProperties merges the provided properties.
func (i *Interaction) SetProperties(ctx context.Context, props Props) error {
	return i.updateState(ctx, func(state *trackRequest) error {
		data, err := normalizeProps(props)
		if err != nil {
			return err
		}

		// Sync to trace attributes if workflow is active
		if i.workflow != nil && !i.workflow.disabled {
			if span := trace.SpanFromContext(i.workflow.Context()); span.IsRecording() {
				for k, v := range data {
					strVal := fmt.Sprintf("%v", v)
					span.SetAttributes(attribute.String("traceloop.association.properties."+k, strVal))
				}
			}
		}

		state.Properties = state.Properties.merge(data)
		return nil
	})
}

// SetInput updates the interaction input for downstream ingestion.
func (i *Interaction) SetInput(ctx context.Context, input string) error {
	return i.updateState(ctx, func(state *trackRequest) error {
		state.ensureAIData().Input = input
		return nil
	})
}

// SetModel updates the AI model metadata.
func (i *Interaction) SetModel(ctx context.Context, model string) error {
	return i.updateState(ctx, func(state *trackRequest) error {
		state.ensureAIData().Model = model
		return nil
	})
}

// AddAttachments appends new attachments to the partial event.
func (i *Interaction) AddAttachments(ctx context.Context, attachments []Attachment) error {
	if err := validateAttachments(attachments); err != nil {
		return err
	}
	return i.updateState(ctx, func(state *trackRequest) error {
		state.Attachments = append(state.Attachments, attachments...)
		return nil
	})
}

// Finish closes the interaction, flushes partials, and enqueues the final TrackAI payload.
func (i *Interaction) Finish(ctx context.Context, params FinishParams) error {
	i.mu.Lock()
	if i.finished {
		i.mu.Unlock()
		return ErrInteractionFinished
	}

	props, err := normalizeProps(params.Properties)
	if err != nil {
		i.mu.Unlock()
		return err
	}
	if params.Output != "" {
		i.state.ensureAIData().Output = params.Output
	}
	if len(params.Attachments) > 0 {
		if err := validateAttachments(params.Attachments); err != nil {
			i.mu.Unlock()
			return err
		}
		i.state.Attachments = append(i.state.Attachments, params.Attachments...)
	}
	if len(props) > 0 {
		i.state.Properties = i.state.Properties.merge(props)
	}

	pendingSnapshot := cloneTrackRequest(i.state)
	pendingSnapshot.IsPending = boolPtr(false)
	eventID := i.state.EventID
	i.finished = true
	if i.timer != nil {
		i.timer.Stop()
	}
	i.mu.Unlock()

	if err := i.client.enqueue(ctx, envelope{
		endpoint:  "events/track_partial",
		payload:   pendingSnapshot,
		ctx:       ctx,
		immediate: true,
	}); err != nil {
		return err
	}

	i.client.partialMu.Lock()
	delete(i.client.partials, eventID)
	i.client.partialMu.Unlock()

	i.closeWorkflow()

	return nil
}

// TrackSignal emits a signal tied to this interaction without passing the event id again.
func (i *Interaction) TrackSignal(ctx context.Context, params SignalParams) error {
	params.EventID = i.EventID()
	return i.client.TrackSignal(ctx, params)
}

func (i *Interaction) updateState(ctx context.Context, mutate func(*trackRequest) error) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.finished {
		return ErrInteractionFinished
	}
	if err := mutate(i.state); err != nil {
		return err
	}
	i.schedulePartialLocked(ctx)
	return nil
}

func (i *Interaction) schedulePartialLocked(ctx context.Context) {
	if i.timer == nil {
		i.timer = time.AfterFunc(i.interval, func() {
			_ = i.flushPartial(context.Background())
		})
		return
	}
	i.timer.Reset(i.interval)
}

func (i *Interaction) flushPartial(ctx context.Context) error {
	i.mu.Lock()
	if i.finished {
		i.mu.Unlock()
		return nil
	}
	snapshot := cloneTrackRequest(i.state)
	snapshot.IsPending = boolPtr(true)
	i.mu.Unlock()

	return i.client.enqueue(ctx, envelope{
		endpoint:  "events/track_partial",
		payload:   snapshot,
		ctx:       ctx,
		immediate: true,
	})
}

func (state *trackRequest) ensureAIData() *aiData {
	if state.AIData == nil {
		state.AIData = &aiData{}
	}
	return state.AIData
}

func cloneTrackRequest(src *trackRequest) *trackRequest {
	if src == nil {
		return nil
	}
	dst := *src
	if src.Properties != nil {
		props := make(Props, len(src.Properties))
		for k, v := range src.Properties {
			props[k] = v
		}
		dst.Properties = props
	}
	if src.Attachments != nil {
		dst.Attachments = append([]Attachment(nil), src.Attachments...)
	}
	if src.AIData != nil {
		copyData := *src.AIData
		dst.AIData = &copyData
	}
	return &dst
}

type LLMSpanConfig struct {
	Prompt Prompt
}

// LLMSpan creates a tracing span for a single model invocation associated with this interaction.
func (i *Interaction) LLMSpan(_ context.Context, cfg LLMSpanConfig) *LLMSpan {
	if cfg.Prompt.Model == "" && i.state.AIData != nil && i.state.AIData.Model != "" {
		cfg.Prompt.Model = i.state.AIData.Model
	}
	if cfg.Prompt.Mode == "" {
		cfg.Prompt.Mode = "chat"
	}
	if cfg.Prompt.Vendor == "" {
		cfg.Prompt.Vendor = "custom"
	}

	// Error is swallowed here to provide a cleaner API (no-op span returned on error/disabled)
	span := i.workflow.LLMSpan(cfg.Prompt)
	return span
}

// TaskSpan creates a child task span under the interaction workflow.
func (i *Interaction) TaskSpan(_ context.Context, cfg TaskSpanConfig) *TaskSpan {
	return i.workflow.NewTask(cfg)
}

// ToolSpan creates a child tool span under the interaction workflow.
func (i *Interaction) ToolSpan(_ context.Context, cfg ToolSpanConfig) *ToolSpan {
	return i.workflow.NewTool(cfg)
}

func (i *Interaction) closeWorkflow() {
	if i.workflow != nil {
		i.workflow.End()
		// We don't nil it out as it might be accessed after finish (though it shouldn't be used for new spans)
		// but setting to nil would race without lock if we did locking.
		// Since finished=true prevents updates, this is fine.
	}
}
