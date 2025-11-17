package raindrop

import (
	"context"
	"testing"
	"time"
)

func TestInteractionMutators(t *testing.T) {
	client := &Client{
		queue:  make(chan envelope, 10),
		ctx:    context.Background(),
		cancel: func() {},
	}
	interaction := newInteraction(client, &trackRequest{
		EventID:    "evt-123",
		Properties: Props{},
	}, time.Hour)

	ctx := context.Background()
	if err := interaction.SetProperty(ctx, "stage", "processing"); err != nil {
		t.Fatalf("SetProperty error: %v", err)
	}
	if err := interaction.SetProperties(ctx, Props{"foo": "bar"}); err != nil {
		t.Fatalf("SetProperties error: %v", err)
	}
	if err := interaction.SetInput(ctx, "hello"); err != nil {
		t.Fatalf("SetInput error: %v", err)
	}
	if err := interaction.SetModel(ctx, "gpt-4"); err != nil {
		t.Fatalf("SetModel error: %v", err)
	}
	if err := interaction.AddAttachments(ctx, []Attachment{{Type: AttachmentTypeText, Role: AttachmentRoleInput, Value: "hi"}}); err != nil {
		t.Fatalf("AddAttachments error: %v", err)
	}

	interaction.mu.Lock()
	defer interaction.mu.Unlock()
	if interaction.state.Properties["stage"] != "processing" {
		t.Fatalf("property not set: %#v", interaction.state.Properties)
	}
	if interaction.state.Properties["foo"] != "bar" {
		t.Fatalf("properties not merged: %#v", interaction.state.Properties)
	}
	if interaction.state.AIData.Input != "hello" {
		t.Fatalf("input not set: %#v", interaction.state.AIData)
	}
	if interaction.state.AIData.Model != "gpt-4" {
		t.Fatalf("model not set: %#v", interaction.state.AIData)
	}
	if len(interaction.state.Attachments) != 1 || interaction.state.Attachments[0].Value != "hi" {
		t.Fatalf("attachment not appended: %#v", interaction.state.Attachments)
	}

	if interaction.timer != nil {
		interaction.timer.Stop()
	}
}

func TestInteractionTrackSignalInjectsEventID(t *testing.T) {
	client := &Client{
		queue:  make(chan envelope, 1),
		ctx:    context.Background(),
		cancel: func() {},
	}
	interaction := newInteraction(client, &trackRequest{
		EventID: "evt-999",
	}, time.Hour)

	if err := interaction.TrackSignal(context.Background(), SignalParams{Name: "thumbs"}); err != nil {
		t.Fatalf("TrackSignal error: %v", err)
	}

	env := <-client.queue
	req, ok := env.payload.(*signalRequest)
	if !ok {
		t.Fatalf("expected signalRequest payload, got %T", env.payload)
	}
	if req.EventID != "evt-999" {
		t.Fatalf("expected interaction event id injected, got %s", req.EventID)
	}
}
