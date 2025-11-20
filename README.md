# Raindrop Go SDK

## Install

```bash
go get github.com/invisible-tools/raindrop-go/raindrop
```

Requires Go 1.24+.

## Usage

Set `RAINDROP_API_KEY` in your environment, then follow the steps below (mirrors `sample-app/main.go`).

### 1. Create the client

```go
ctx := context.Background()
client, err := raindrop.NewClient(ctx, raindrop.Config{
	WriteKey:      os.Getenv("RAINDROP_API_KEY"),
	FlushInterval: time.Second,
	Debug:         true,
	Tracing: raindrop.TracingConfig{
		Enabled:     true,
		ServiceName: "sample-app",
	},
})
if err != nil {
	log.Fatalf("init raindrop: %v", err)
}
defer client.Shutdown(ctx)
```

`Tracing` is optional—leave it disabled if you only care about ingesting events.

### 2. Capture AI interactions

```go
interaction, err := client.Begin(ctx, raindrop.BeginParams{
	Event:  "sentiment_demo",
	UserID: "user-123",
	Input:  "Analyze sentiment of customer feedback",
	Model:  "gpt-4o-mini",
	Properties: raindrop.Props{
		"stage": "started",
	},
})
if err != nil {
	log.Fatalf("begin interaction: %v", err)
}

_ = interaction.SetProperty(ctx, "stage", "calling_openai")
_ = interaction.AddAttachments(ctx, []raindrop.Attachment{{
	Type:  raindrop.AttachmentTypeText,
	Role:  raindrop.AttachmentRoleInput,
	Name:  "customer_feedback.txt",
	Value: "I loved support but I'm confused by the billing portal.",
}})

if err := interaction.Finish(ctx, raindrop.FinishParams{
	Output: "Thanks for the feedback! Here’s how to navigate the portal…",
	Properties: raindrop.Props{
		"stage": "completed",
	},
}); err != nil {
	log.Fatalf("finish interaction: %v", err)
}
```

Call `Begin` to create the interaction, enrich it with properties and attachments as you go, and call `Finish` once you have the final output.

### 3. Gather human feedback & signals

```go
if err := client.TrackSignal(ctx, raindrop.SignalParams{
	EventID:   interaction.EventID(),
	Name:      "thumbs_down",
	Type:      raindrop.SignalTypeFeedback,
	Sentiment: raindrop.SentimentNegative,
	Comment:   "Output missed the billing details",
}); err != nil {
	log.Printf("track signal: %v", err)
}
```

Signals tie QA reviews, customer comments, or tool automation directly back to the interaction so you can train or route follow-ups.

### 4. Trace AI flows (optional)

```go
// 1. Tasks group logical steps
task := interaction.TaskSpan(ctx, raindrop.TaskSpanConfig{Name: "sentiment.analysis"})
defer task.End()

// IMPORTANT: Use task.Context() for nested spans to maintain the trace hierarchy
toolCtx := task.Context()

// 2. Tools capture external calls with input/output
tool := interaction.ToolSpan(toolCtx, raindrop.ToolSpanConfig{
    Name: "playbook_lookup",
    Type: "function",
    Function: raindrop.ToolFunction{
        Name:        "playbook_lookup",
        Description: "Finds recommended actions",
        Parameters:  map[string]any{"sentiment": "negative"},
    },
})
defer tool.End()

// ... perform tool logic ...
tool.ReportResult("playbook_id: retention_risk")

// 3. LLM spans track model usage
llmSpan := interaction.LLMSpan(toolCtx, raindrop.LLMSpanConfig{
	Prompt: raindrop.Prompt{
		Vendor: "openai",
		Mode:   "chat",
		Model:  "gpt-4o-mini",
		Messages: []raindrop.Message{
			{Index: 0, Role: "system", Content: systemPrompt},
			{Index: 1, Role: "user", Content: "Analyze this feedback…"},
		},
	},
})
defer llmSpan.End(ctx)

llmSpan.RecordCompletion(ctx, raindrop.Completion{
	Model: "gpt-4o-mini",
}, raindrop.Usage{
	PromptTokens:     int(usage.PromptTokens),
	CompletionTokens: int(usage.CompletionTokens),
	TotalTokens:      int(usage.TotalTokens),
})
```

When tracing is enabled, every workflow, task, and model call shows up in Traceloop with token counts, prompts, completions, and attachments.

## Sample application (`sample-app/`)

Clone this repository, set your API keys, and run the sample to see each capability in action.

### Run it

```bash
cd sample-app
RAINDROP_API_KEY=your_raindrop_key OPENAI_API_KEY=sk-... go run .
```

