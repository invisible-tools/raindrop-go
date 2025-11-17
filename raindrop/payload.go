package raindrop

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
)

// Props mirrors the JavaScript SDK map contract (string | int | bool).
type Props map[string]any

func (p Props) clone() Props {
	if len(p) == 0 {
		return nil
	}
	cp := make(Props, len(p))
	for k, v := range p {
		cp[k] = v
	}
	return cp
}

func (p Props) merge(other Props) Props {
	if len(other) == 0 {
		return p
	}
	if p == nil {
		p = Props{}
	}
	for k, v := range other {
		p[k] = v
	}
	return p
}

func normalizeProps(props Props) (Props, error) {
	if len(props) == 0 {
		return nil, nil
	}
	out := make(Props, len(props))
	for k, v := range props {
		switch val := v.(type) {
		case string:
			out[k] = val
		case bool:
			out[k] = val
		case fmt.Stringer:
			out[k] = val.String()
		default:
			if i64, ok := toInt64(val); ok {
				out[k] = i64
				continue
			}
			return nil, ValidationError{
				Field: fmt.Sprintf("properties.%s", k),
				Msg:   fmt.Sprintf("unsupported type %s (allowed: string | int | bool)", reflect.TypeOf(v)),
			}
		}
	}
	return out, nil
}

func toInt64(v any) (int64, bool) {
	switch val := v.(type) {
	case int:
		return int64(val), true
	case int8:
		return int64(val), true
	case int16:
		return int64(val), true
	case int32:
		return int64(val), true
	case int64:
		return val, true
	case uint:
		if val > math.MaxInt64 {
			return 0, false
		}
		return int64(val), true
	case uint8:
		return int64(val), true
	case uint16:
		return int64(val), true
	case uint32:
		return int64(val), true
	case uint64:
		if val > math.MaxInt64 {
			return 0, false
		}
		return int64(val), true
	default:
		return 0, false
	}
}

type AttachmentType string

const (
	AttachmentTypeText   AttachmentType = "text"
	AttachmentTypeCode   AttachmentType = "code"
	AttachmentTypeImage  AttachmentType = "image"
	AttachmentTypeIframe AttachmentType = "iframe"
)

type AttachmentRole string

const (
	AttachmentRoleInput  AttachmentRole = "input"
	AttachmentRoleOutput AttachmentRole = "output"
)

type Attachment struct {
	Type     AttachmentType `json:"type"`
	Role     AttachmentRole `json:"role"`
	Name     string         `json:"name,omitempty"`
	Value    string         `json:"value"`
	Language string         `json:"language,omitempty"`
}

func validateAttachments(list []Attachment) error {
	for idx, att := range list {
		if att.Type == "" {
			return ValidationError{Field: fmt.Sprintf("attachments[%d].type", idx), Msg: "type is required"}
		}
		switch att.Type {
		case AttachmentTypeText, AttachmentTypeCode, AttachmentTypeImage, AttachmentTypeIframe:
		default:
			return ValidationError{Field: fmt.Sprintf("attachments[%d].type", idx), Msg: "unknown attachment type"}
		}
		if att.Role == "" {
			return ValidationError{Field: fmt.Sprintf("attachments[%d].role", idx), Msg: "role is required"}
		}
		switch att.Role {
		case AttachmentRoleInput, AttachmentRoleOutput:
		default:
			return ValidationError{Field: fmt.Sprintf("attachments[%d].role", idx), Msg: "unknown attachment role"}
		}
		if strings.TrimSpace(att.Value) == "" {
			return ValidationError{Field: fmt.Sprintf("attachments[%d].value", idx), Msg: "value is required"}
		}
		if att.Type != AttachmentTypeCode && att.Language != "" {
			return ValidationError{Field: fmt.Sprintf("attachments[%d].language", idx), Msg: "language is only valid for code attachments"}
		}
	}
	return nil
}

type BeginParams struct {
	Event       string
	EventID     string
	UserID      string
	Input       string
	Model       string
	ConvoID     string
	Properties  Props
	Attachments []Attachment
}

type FinishParams struct {
	Output      string
	Properties  Props
	Attachments []Attachment
}

type IdentifyParams struct {
	UserID string
	Traits map[string]any
}

type SignalSentiment string

const (
	SentimentPositive SignalSentiment = "POSITIVE"
	SentimentNegative SignalSentiment = "NEGATIVE"
)

type SignalType string

const (
	SignalTypeDefault  SignalType = "default"
	SignalTypeFeedback SignalType = "feedback"
	SignalTypeEdit     SignalType = "edit"
	SignalTypeStandard SignalType = "standard"
)

type SignalParams struct {
	EventID      string
	Name         string
	Timestamp    time.Time
	Sentiment    SignalSentiment
	Type         SignalType
	Comment      string
	After        string
	Properties   Props
	AttachmentID string
}

type trackRequest struct {
	EventID     string       `json:"event_id"`
	Event       string       `json:"event"`
	UserID      string       `json:"user_id"`
	Timestamp   time.Time    `json:"timestamp"`
	AIData      *aiData      `json:"ai_data,omitempty"`
	Properties  Props        `json:"properties,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	IsPending   *bool        `json:"is_pending,omitempty"`
}

type aiData struct {
	Model  string `json:"model,omitempty"`
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
	Convo  string `json:"convo_id,omitempty"`
}

func buildAIData(input, output, model, convo string) *aiData {
	data := &aiData{
		Model:  model,
		Input:  input,
		Output: output,
		Convo:  convo,
	}
	if data.Model == "" && data.Input == "" && data.Output == "" && data.Convo == "" {
		return nil
	}
	return data
}

type identifyRequest struct {
	UserID string         `json:"user_id"`
	Traits map[string]any `json:"traits"`
}

func (p IdentifyParams) build() (*identifyRequest, error) {
	if strings.TrimSpace(p.UserID) == "" {
		return nil, ValidationError{Field: "userId", Msg: "userId is required"}
	}
	if p.Traits == nil {
		p.Traits = map[string]any{}
	}
	return &identifyRequest{UserID: p.UserID, Traits: p.Traits}, nil
}

type signalRequest struct {
	EventID      string    `json:"event_id"`
	Name         string    `json:"signal_name"`
	Timestamp    time.Time `json:"timestamp,omitempty"`
	Properties   Props     `json:"properties,omitempty"`
	AttachmentID string    `json:"attachment_id,omitempty"`
	Sentiment    string    `json:"sentiment,omitempty"`
	Type         string    `json:"signal_type,omitempty"`
}

func (p SignalParams) build(ctxProps Props) (*signalRequest, error) {
	if strings.TrimSpace(p.EventID) == "" {
		return nil, ValidationError{Field: "eventId", Msg: "eventId is required"}
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, ValidationError{Field: "name", Msg: "signal name is required"}
	}
	props, err := normalizeProps(p.Properties)
	if err != nil {
		return nil, err
	}
	if p.Comment != "" {
		props = props.merge(Props{"comment": p.Comment})
	}
	if p.After != "" {
		props = props.merge(Props{"after": p.After})
	}
	req := &signalRequest{
		EventID:      p.EventID,
		Name:         p.Name,
		Properties:   mergeContext(props, ctxProps),
		AttachmentID: p.AttachmentID,
	}
	if !p.Timestamp.IsZero() {
		req.Timestamp = p.Timestamp.UTC()
	}
	if p.Sentiment != "" {
		req.Sentiment = string(p.Sentiment)
	}
	if p.Type != "" {
		req.Type = string(p.Type)
	}
	return req, ensurePayloadLimit(req)
}

func ensurePayloadLimit(body any) error {
	if body == nil {
		return nil
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("raindrop: failed to marshal payload: %w", err)
	}
	if len(data) > maxPayloadBytes {
		return ValidationError{Msg: fmt.Sprintf("payload exceeds %d bytes", maxPayloadBytes)}
	}
	return nil
}

func mergeContext(props Props, ctx Props) Props {
	if len(ctx) == 0 {
		if len(props) == 0 {
			return nil
		}
		return props
	}
	if props == nil {
		props = Props{}
	}
	dst := Props{}
	for k, v := range ctx {
		dst[k] = v
	}
	if existing, ok := props["$context"].(map[string]any); ok {
		for k, v := range existing {
			dst[k] = v
		}
	}
	props["$context"] = dst
	return props
}
