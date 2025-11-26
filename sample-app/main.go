package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	raindrop "github.com/invisible-tools/raindrop-go/raindrop"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const systemPrompt = "" +
	"You are a customer experience analyst. " +
	"Return a JSON object with fields sentiment (positive|negative|neutral), " +
	"confidence (0-1 float), keywords (array of 3 focus phrases), and summary (single sentence)."

const followupSystemPrompt = "" +
	"You are a lifecycle manager drafting concise follow-up notes for a customer. " +
	"Respond in JSON with fields subject (string), body (string), next_steps (array of strings), and tone (string)."

var sampleFeedback = `I loved how quickly support responded, but I was confused about the new billing portal.
The UI changes felt rushed and I had to ping our rep twice before understanding the upgrade path.`

type SentimentResult struct {
	Sentiment  string   `json:"sentiment"`
	Confidence float64  `json:"confidence"`
	Keywords   []string `json:"keywords"`
	Summary    string   `json:"summary"`
}

type PlaybookRecommendation struct {
	Segment           string `json:"segment"`
	NextBestAction    string `json:"next_best_action"`
	ConfidenceLevel   string `json:"confidence_level"`
	KnowledgeBaseLink string `json:"knowledge_base_link"`
}

type FollowupPlan struct {
	Subject   string   `json:"subject"`
	Body      string   `json:"body"`
	NextSteps []string `json:"next_steps"`
	Tone      string   `json:"tone"`
}

func main() {
	ctx := context.Background()

	raindropClient, err := raindrop.NewClient(ctx, raindrop.Config{
		WriteKey:      os.Getenv("RAINDROP_API_KEY"),
		Debug:         true,
		FlushInterval: time.Second,
		Tracing: raindrop.TracingConfig{
			Enabled:     true,
			ServiceName: "sample-app",
		},
	})
	if err != nil {
		log.Fatalf("init client: %v", err)
	}
	defer raindropClient.Shutdown(ctx)

	apiKey := os.Getenv("OPENAI_API_KEY")
	if strings.TrimSpace(apiKey) == "" {
		log.Fatal("OPENAI_API_KEY must be set")
	}

	openaiClient := openai.NewClient(option.WithAPIKey(apiKey))

	fmt.Printf("Raindrop SDK version: %s\n", raindrop.Version())

	sentimentPrompt := buildSentimentPrompt(sampleFeedback)
	openaiSentimentMessages, raindropSentimentMessages := buildChatPrompts(systemPrompt, sentimentPrompt)

	interaction, err := raindropClient.Begin(ctx, raindrop.BeginParams{
		Event:   "sentiment_demo",
		UserID:  "user-123",
		ConvoID: fmt.Sprintf("conv-%d", time.Now().Unix()),
		Input:   "Analyze sentiment of customer feedback",
		Model:   string(openai.ChatModelGPT4oMini),
		Properties: raindrop.Props{
			"stage": "started",
		},
	})
	if err != nil {
		log.Fatalf("begin interaction: %v", err)
	}

	if err := interaction.AddAttachments(ctx, []raindrop.Attachment{{
		Type:  raindrop.AttachmentTypeText,
		Role:  raindrop.AttachmentRoleInput,
		Name:  "customer_feedback.txt",
		Value: sampleFeedback,
	}}); err != nil {
		log.Fatalf("add input attachment: %v", err)
	}

	sentimentTask := interaction.TaskSpan(ctx, raindrop.TaskSpanConfig{Name: "sentiment.analysis"})
	defer sentimentTask.End()

	llmSpan := interaction.LLMSpan(ctx, raindrop.LLMSpanConfig{
		Prompt: raindrop.Prompt{
			Vendor:   "openai",
			Mode:     "chat",
			Model:    string(openai.ChatModelGPT4oMini),
			Messages: raindropSentimentMessages,
		},
	})
	defer llmSpan.End(ctx)

	if err := interaction.SetProperty(ctx, "stage", "calling_openai"); err != nil {
		log.Fatalf("set property: %v", err)
	}

	result, rawAssistant, usage, err := analyzeSentiment(ctx, openaiClient, openaiSentimentMessages)
	if err != nil {
		log.Fatalf("sentiment analysis: %v", err)
	}

	if err := llmSpan.RecordCompletion(ctx, raindrop.Completion{
		Model: string(openai.ChatModelGPT4oMini),
		Messages: append(raindropSentimentMessages, raindrop.Message{
			Index:   2,
			Role:    "assistant",
			Content: rawAssistant,
		}),
	}, raindrop.Usage{
		PromptTokens:     int(usage.PromptTokens),
		CompletionTokens: int(usage.CompletionTokens),
		TotalTokens:      int(usage.TotalTokens),
	}); err != nil {
		log.Printf("record completion: %v", err)
	}

	if err := interaction.SetProperties(ctx, raindrop.Props{
		"stage":              "sentiment_ready",
		"sentiment":          result.Sentiment,
		"confidence_pct":     int(math.Round(result.Confidence * 100)),
		"keywords_detected":  len(result.Keywords),
		"positive_sentiment": result.Sentiment == "positive",
	}); err != nil {
		log.Fatalf("set properties: %v", err)
	}

	if err := interaction.AddAttachments(ctx, []raindrop.Attachment{{
		Type:     raindrop.AttachmentTypeCode,
		Role:     raindrop.AttachmentRoleOutput,
		Name:     "sentiment_result.json",
		Value:    formatJSON(result),
		Language: "json",
	}}); err != nil {
		log.Fatalf("add output attachment: %v", err)
	}

	// IMPORTANT: Capture context from parent task to maintain hierarchy
	playbookTask := interaction.ToolSpan(sentimentTask.Context(), raindrop.ToolSpanConfig{
		Name: "playbook_lookup",
		Type: "function",
		Function: raindrop.ToolFunction{
			Name:        "playbook_lookup",
			Description: "Looks up recommended actions based on customer feedback sentiment.",
			Parameters: map[string]any{
				"sentiment":       result.Sentiment,
				"keywords":        result.Keywords,
				"contains_portal": strings.Contains(strings.ToLower(sampleFeedback), "billing"),
			},
		},
	})
	defer playbookTask.End()
	playbook := lookupPlaybook(sampleFeedback, result)
	playbookTask.ReportResult(formatJSON(playbook))

	if err := interaction.SetProperties(ctx, raindrop.Props{
		"stage":             "playbook_ready",
		"playbook_segment":  playbook.Segment,
		"playbook_conf":     playbook.ConfidenceLevel,
		"playbook_resource": playbook.KnowledgeBaseLink,
	}); err != nil {
		log.Fatalf("set playbook properties: %v", err)
	}

	if err := interaction.AddAttachments(ctx, []raindrop.Attachment{{
		Type:     raindrop.AttachmentTypeCode,
		Role:     raindrop.AttachmentRoleOutput,
		Name:     "playbook_recommendation.json",
		Value:    formatJSON(playbook),
		Language: "json",
	}}); err != nil {
		log.Fatalf("add playbook attachment: %v", err)
	}

	// Example: Failed tool call span
	customerHistoryTask := interaction.ToolSpan(sentimentTask.Context(), raindrop.ToolSpanConfig{
		Name: "customer_history_lookup",
		Type: "function",
		Function: raindrop.ToolFunction{
			Name:        "customer_history_lookup",
			Description: "Retrieves customer interaction history from CRM system",
			Parameters: map[string]any{
				"user_id": "user-123",
				"limit":   10,
			},
		},
	})
	defer customerHistoryTask.End()

	// Simulate a failed API call
	if err := simulateFailedCustomerHistoryLookup(); err != nil {
		log.Printf("customer history lookup failed: %v", err)
		customerHistoryTask.ReportError(err)
		// Continue execution despite the error
	}

	followupPrompt := buildFollowupPrompt(sampleFeedback, result, playbook)
	openaiFollowupMessages, raindropFollowupMessages := buildChatPrompts(followupSystemPrompt, followupPrompt)

	followupSpan := interaction.LLMSpan(ctx, raindrop.LLMSpanConfig{
		Prompt: raindrop.Prompt{
			Vendor:   "openai",
			Mode:     "chat",
			Model:    string(openai.ChatModelGPT4oMini),
			Messages: raindropFollowupMessages,
		},
	})
	defer followupSpan.End(ctx)

	plan, planRaw, followupUsage, err := draftFollowupPlan(ctx, openaiClient, openaiFollowupMessages)
	if err != nil {
		log.Fatalf("draft follow-up: %v", err)
	}

	if err := followupSpan.RecordCompletion(ctx, raindrop.Completion{
		Model: string(openai.ChatModelGPT4oMini),
		Messages: append(raindropFollowupMessages, raindrop.Message{
			Index:   2,
			Role:    "assistant",
			Content: planRaw,
		}),
	}, raindrop.Usage{
		PromptTokens:     int(followupUsage.PromptTokens),
		CompletionTokens: int(followupUsage.CompletionTokens),
		TotalTokens:      int(followupUsage.TotalTokens),
	}); err != nil {
		log.Printf("record follow-up completion: %v", err)
	}

	if err := interaction.AddAttachments(ctx, []raindrop.Attachment{{
		Type:     raindrop.AttachmentTypeCode,
		Role:     raindrop.AttachmentRoleOutput,
		Name:     "followup_plan.json",
		Value:    formatJSON(plan),
		Language: "json",
	}}); err != nil {
		log.Fatalf("add follow-up attachment: %v", err)
	}

	if err := interaction.SetProperties(ctx, raindrop.Props{
		"stage":            "completed",
		"followup_subject": plan.Subject,
		"followup_tone":    plan.Tone,
		"next_steps_count": len(plan.NextSteps),
	}); err != nil {
		log.Fatalf("set follow-up properties: %v", err)
	}

	finalUsage := addUsage(usage, followupUsage)
	finalOutput := plan.Body
	if strings.TrimSpace(finalOutput) == "" {
		finalOutput = result.Summary
	}

	if err := interaction.Finish(ctx, raindrop.FinishParams{
		Output: finalOutput,
		Properties: raindrop.Props{
			"stage": "completed",
		},
	}); err != nil {
		log.Fatalf("finish interaction: %v", err)
	}

	fmt.Printf("Sentiment: %s (%.0f%% confidence)\n", result.Sentiment, result.Confidence*100)
	fmt.Printf("Keywords: %v\n", result.Keywords)
	fmt.Printf("Playbook segment: %s next action: %s\n", playbook.Segment, playbook.NextBestAction)
	fmt.Printf("Follow-up subject: %s\n", plan.Subject)
	if len(plan.NextSteps) > 0 {
		fmt.Printf("Next steps:\n")
		for _, step := range plan.NextSteps {
			fmt.Printf("  - %s\n", step)
		}
	}
	fmt.Printf("OpenAI tokens (combined) - prompt:%d completion:%d total:%d\n", finalUsage.PromptTokens, finalUsage.CompletionTokens, finalUsage.TotalTokens)
	fmt.Printf("Interaction %s finished via track_partial\n", interaction.EventID())
}

func analyzeSentiment(ctx context.Context, client openai.Client, messages []openai.ChatCompletionMessageParamUnion) (SentimentResult, string, openai.CompletionUsage, error) {
	jsonFormat := shared.NewResponseFormatJSONObjectParam()

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModelGPT4oMini,
		Messages: messages,
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &jsonFormat,
		},
		Temperature: openai.Float(0.2),
	}

	completion, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		return SentimentResult{}, "", openai.CompletionUsage{}, fmt.Errorf("openai chat completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return SentimentResult{}, "", completion.Usage, fmt.Errorf("openai chat completion: no choices returned")
	}

	content := completion.Choices[0].Message.Content
	var parsed SentimentResult
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return SentimentResult{}, content, completion.Usage, fmt.Errorf("parse sentiment response: %w", err)
	}

	return parsed, content, completion.Usage, nil
}

func buildChatPrompts(system, user string) ([]openai.ChatCompletionMessageParamUnion, []raindrop.Message) {
	openaiMessages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(system),
		openai.UserMessage(user),
	}
	raindropMessages := []raindrop.Message{
		{Index: 0, Role: "system", Content: system},
		{Index: 1, Role: "user", Content: user},
	}
	return openaiMessages, raindropMessages
}

func buildSentimentPrompt(feedback string) string {
	return fmt.Sprintf(
		"Determine sentiment for the following customer feedback. "+
			"Base the confidence on how unambiguous the wording feels. Text: %q",
		feedback,
	)
}

func lookupPlaybook(feedback string, sentiment SentimentResult) PlaybookRecommendation {
	segment := "growth"
	confidence := "medium"
	action := "Thank them for the positive feedback and invite them to the roadmap webinar."

	switch sentiment.Sentiment {
	case "negative":
		segment = "retention_risk"
		confidence = "high"
		action = "Offer a guided session that walks through the billing portal changes."
	case "neutral":
		segment = "watchlist"
		action = "Share a short loom explaining the billing portal and ask for async comments."
	}

	if strings.Contains(strings.ToLower(feedback), "billing") {
		action = "Send the billing portal walkthrough and schedule a migration check-in."
	}

	return PlaybookRecommendation{
		Segment:           segment,
		NextBestAction:    action,
		ConfidenceLevel:   confidence,
		KnowledgeBaseLink: fmt.Sprintf("https://example.com/playbooks/%s", segment),
	}
}

func simulateFailedCustomerHistoryLookup() error {
	// Simulate a timeout or connection error from an external CRM API
	return fmt.Errorf("CRM API timeout: connection to customer-history-service failed after 5s")
}

func buildFollowupPrompt(feedback string, sentiment SentimentResult, playbook PlaybookRecommendation) string {
	return fmt.Sprintf(
		"Write a short follow-up to the customer using the following context:\n"+
			"- Feedback: %q\n- Sentiment: %s (%.0f%% confidence)\n- Keywords: %v\n- Recommended action: %s\n"+
			"Return JSON with subject, body, next_steps (array) and tone.",
		feedback,
		sentiment.Sentiment,
		sentiment.Confidence*100,
		sentiment.Keywords,
		playbook.NextBestAction,
	)
}

func draftFollowupPlan(ctx context.Context, client openai.Client, messages []openai.ChatCompletionMessageParamUnion) (FollowupPlan, string, openai.CompletionUsage, error) {
	jsonFormat := shared.NewResponseFormatJSONObjectParam()
	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModelGPT4oMini,
		Messages: messages,
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &jsonFormat,
		},
		Temperature: openai.Float(0.4),
	}

	completion, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		return FollowupPlan{}, "", openai.CompletionUsage{}, fmt.Errorf("openai follow-up completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return FollowupPlan{}, "", completion.Usage, fmt.Errorf("openai follow-up completion: no choices returned")
	}

	content := completion.Choices[0].Message.Content
	var plan FollowupPlan
	if err := json.Unmarshal([]byte(content), &plan); err != nil {
		return FollowupPlan{}, content, completion.Usage, fmt.Errorf("parse follow-up response: %w", err)
	}

	return plan, content, completion.Usage, nil
}

func addUsage(a, b openai.CompletionUsage) openai.CompletionUsage {
	return openai.CompletionUsage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
	}
}

func formatJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(data)
}
