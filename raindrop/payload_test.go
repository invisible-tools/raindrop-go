package raindrop

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestNormalizePropsAcceptsSupportedTypes(t *testing.T) {
	props, err := normalizeProps(Props{
		"string": "value",
		"bool":   true,
		"int":    int32(10),
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if props["string"] != "value" || props["bool"] != true {
		t.Fatalf("unexpected props: %#v", props)
	}
	if props["int"] != int64(10) {
		t.Fatalf("integers should be normalized to int64, got %#v", props["int"])
	}
}

func TestNormalizePropsRejectsUnsupportedTypes(t *testing.T) {
	_, err := normalizeProps(Props{
		"bad": []string{"nope"},
	})
	var ve ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
}

func TestValidateAttachments(t *testing.T) {
	err := validateAttachments([]Attachment{
		{Type: AttachmentTypeText, Role: AttachmentRoleInput, Value: "hello"},
	})
	if err != nil {
		t.Fatalf("expected attachment to pass validation: %v", err)
	}

	err = validateAttachments([]Attachment{
		{Type: AttachmentTypeText, Role: AttachmentRoleInput, Value: ""},
	})
	if err == nil {
		t.Fatalf("expected empty value to fail validation")
	}
}

func TestSignalParamsBuild(t *testing.T) {
	ctxProps := Props{
		"library": "go",
	}
	req, err := (SignalParams{
		EventID: "evt",
		Name:    "thumbs_up",
		Comment: "nice",
		After:   "reply",
	}).build(ctxProps)
	if err != nil {
		t.Fatalf("build error: %v", err)
	}
	if req.EventID != "evt" || req.Name != "thumbs_up" {
		t.Fatalf("unexpected request %#v", req)
	}
	if req.Properties["comment"] != "nice" || req.Properties["after"] != "reply" {
		t.Fatalf("expected comment and after properties: %#v", req.Properties)
	}
	ctxValue, ok := req.Properties["$context"].(Props)
	if !ok || ctxValue["library"] != "go" {
		t.Fatalf("expected context properties to merge, got %#v", req.Properties["$context"])
	}
}

func TestEnsurePayloadLimit(t *testing.T) {
	body := struct {
		Data string `json:"data"`
	}{
		Data: string(make([]byte, maxPayloadBytes+1)),
	}
	if err := ensurePayloadLimit(body); err == nil {
		t.Fatalf("expected payload larger than 1MB to fail")
	}
}

func TestInteractionLLMSpanDefaults(t *testing.T) {
	client := &Client{
		tracer: &Tracer{},
	}
	interaction := &Interaction{
		client: client,
		state: &trackRequest{
			Event: "chat",
			AIData: &aiData{
				Model: "gpt-4o-mini",
			},
		},
	}

	span, err := interaction.LLMSpan(context.Background(), LLMSpanConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if span == nil {
		t.Fatalf("expected span even when tracer disabled")
	}
}

func TestTrackRequestCloneIsolation(t *testing.T) {
	orig := &trackRequest{
		EventID: "evt",
		Properties: Props{
			"stage": "start",
		},
		Attachments: []Attachment{
			{Type: AttachmentTypeText, Role: AttachmentRoleInput, Value: "hello"},
		},
		AIData: &aiData{Model: "gpt-4"},
	}

	cloned := cloneTrackRequest(orig)
	cloned.Properties["stage"] = "mutated"
	cloned.Attachments[0].Value = "bye"
	cloned.AIData.Model = "other"

	if orig.Properties["stage"] != "start" {
		t.Fatalf("original properties mutated: %#v", orig.Properties)
	}
	if orig.Attachments[0].Value != "hello" {
		t.Fatalf("original attachment mutated: %#v", orig.Attachments[0])
	}
	if orig.AIData.Model != "gpt-4" {
		t.Fatalf("original aidata mutated: %#v", orig.AIData)
	}
}

func TestSignalParamsRequiresEventID(t *testing.T) {
	if _, err := (SignalParams{Name: "thumbs"}).build(nil); err == nil {
		t.Fatalf("expected error when event id missing")
	}
}

func TestToInt64RejectsOutOfRange(t *testing.T) {
	if _, ok := toInt64(uint64(math.MaxUint64)); ok {
		t.Fatalf("expected uint64 overflow to be rejected")
	}
}
