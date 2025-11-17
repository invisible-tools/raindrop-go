package raindrop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type Client struct {
	cfg        Config
	httpClient *http.Client
	endpoint   string

	queue   chan envelope
	flushCh chan flushRequest

	// ctx/cancel are derived from the caller's context at construction time.
	// They are never the request context itself; we just need a private root
	// to coordinate worker shutdown without storing user-scoped contexts.
	ctx    context.Context
	cancel context.CancelFunc

	workerWG  sync.WaitGroup
	closeOnce sync.Once

	logger *log.Logger
	debug  bool

	contextProps Props

	partials  map[string]*Interaction
	partialMu sync.RWMutex

	tracer *Tracer

	metrics clientMetrics
}

type clientMetrics struct {
	enqueued atomic.Uint64
	dropped  atomic.Uint64
	sent     atomic.Uint64
	failed   atomic.Uint64
}

type envelope struct {
	endpoint  string
	payload   any
	ctx       context.Context
	immediate bool
}

type flushRequest struct {
	ctx  context.Context
	done chan error
}

// NewClient constructs a thread-safe client with the worker goroutine running.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	cctx, cancel := context.WithCancel(ctx)
	client := &Client{
		cfg:          cfg,
		httpClient:   cfg.HTTPClient,
		endpoint:     normalizeEndpoint(cfg.Endpoint),
		queue:        make(chan envelope, cfg.MaxQueueSize),
		flushCh:      make(chan flushRequest),
		ctx:          cctx,
		cancel:       cancel,
		debug:        cfg.Debug,
		logger:       log.New(os.Stdout, "[raindrop] ", log.LstdFlags),
		contextProps: defaultContextProps(),
		partials:     map[string]*Interaction{},
	}

	if !cfg.Tracing.Enabled {
		cfg.Tracing.Enabled = false
	}

	tracer, err := newTracer(ctx, cfg.Tracing, client.endpoint)
	if err != nil {
		return nil, fmt.Errorf("init tracing: %w", err)
	}
	client.tracer = tracer

	client.workerWG.Add(1)
	go client.runWorker()

	return client, nil
}

// Identify sends user traits.
func (c *Client) Identify(ctx context.Context, params IdentifyParams) error {
	req, err := params.build()
	if err != nil {
		return err
	}
	return c.enqueue(ctx, envelope{
		endpoint: "users/identify",
		payload:  req,
		ctx:      ctx,
	})
}

// TrackSignal emits feedback or other signal events.
func (c *Client) TrackSignal(ctx context.Context, params SignalParams) error {
	req, err := params.build(c.contextProps)
	if err != nil {
		return err
	}
	return c.enqueue(ctx, envelope{
		endpoint: "signals/track",
		payload:  req,
		ctx:      ctx,
	})
}

// Tracer exposes the tracing helpers configured for this client.
func (c *Client) Tracer() *Tracer {
	if c == nil {
		return &Tracer{}
	}
	if c.tracer == nil {
		return &Tracer{}
	}
	return c.tracer
}

// Begin starts an interaction by buffering a partial event and returning a handle.
func (c *Client) Begin(ctx context.Context, params BeginParams) (*Interaction, error) {
	if strings.TrimSpace(params.Event) == "" {
		return nil, ValidationError{Field: "event", Msg: "event name is required"}
	}
	if strings.TrimSpace(params.UserID) == "" {
		return nil, ValidationError{Field: "userId", Msg: "userId is required"}
	}
	if err := validateAttachments(params.Attachments); err != nil {
		return nil, err
	}
	props, err := normalizeProps(params.Properties)
	if err != nil {
		return nil, err
	}
	eventID := params.EventID
	if eventID == "" {
		eventID = uuidNewString()
	}

	state := &trackRequest{
		EventID:     eventID,
		Event:       params.Event,
		UserID:      params.UserID,
		Timestamp:   time.Now().UTC(),
		AIData:      buildAIData(params.Input, "", params.Model, params.ConvoID),
		Properties:  mergeContext(props, c.contextProps),
		Attachments: slices.Clone(params.Attachments),
		IsPending:   boolPtr(true),
	}

	interaction := newInteraction(c, state, c.cfg.PartialFlushInterval)

	c.partialMu.Lock()
	c.partials[eventID] = interaction
	c.partialMu.Unlock()

	if err := interaction.flushPartial(ctx); err != nil && c.debug {
		c.logger.Printf("initial partial flush failed: %v", err)
	}

	return interaction, nil
}

// ResumeInteraction returns the Interaction object if it is still tracked.
func (c *Client) ResumeInteraction(eventID string) *Interaction {
	c.partialMu.RLock()
	defer c.partialMu.RUnlock()
	return c.partials[eventID]
}

// Flush blocks until all buffered batches are delivered.
func (c *Client) Flush(ctx context.Context) error {
	req := flushRequest{
		ctx:  ctx,
		done: make(chan error, 1),
	}
	select {
	case c.flushCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return errors.New("raindrop: client shutting down")
	}

	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return errors.New("raindrop: client shutting down")
	}
}

// Shutdown flushes partial interactions, stops the worker and closes tracing.
func (c *Client) Shutdown(ctx context.Context) error {
	var shutdownErr error

	c.closeOnce.Do(func() {
		c.partialMu.Lock()
		for _, inter := range c.partials {
			if err := inter.flushPartial(context.Background()); err != nil && shutdownErr == nil {
				shutdownErr = err
			}
		}
		c.partialMu.Unlock()

		close(c.queue)
		close(c.flushCh)
	})

	done := make(chan struct{})
	go func() {
		c.workerWG.Wait()
		close(done)
	}()

	timedOut := false
	select {
	case <-done:
	case <-ctx.Done():
		timedOut = true
		c.cancel()
		<-done
	}

	if !timedOut {
		c.cancel()
	}

	if c.tracer != nil {
		if err := c.tracer.Close(ctx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}

	return shutdownErr
}

// Health snapshot for debugging.
func (c *Client) Health() map[string]any {
	return map[string]any{
		"queued":   len(c.queue),
		"enqueued": c.metrics.enqueued.Load(),
		"dropped":  c.metrics.dropped.Load(),
		"sent":     c.metrics.sent.Load(),
		"failed":   c.metrics.failed.Load(),
	}
}

func (c *Client) enqueue(ctx context.Context, env envelope) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case <-c.ctx.Done():
		return errors.New("raindrop: client is closed")
	default:
	}

	select {
	case c.queue <- env:
		c.metrics.enqueued.Add(1)
		return nil
	default:
		c.metrics.dropped.Add(1)
		return ErrQueueFull
	}
}

func (c *Client) runWorker() {
	defer c.workerWG.Done()

	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()

	buffers := map[string][]any{}

	for {
		select {
		case env, ok := <-c.queue:
			if !ok {
				c.flushBuffers(context.Background(), buffers)
				return
			}
			if env.immediate {
				if err := c.sendPayloadAttempts(env.ctx, env.endpoint, env.payload, 1); err != nil {
					c.metrics.failed.Add(1)
					if c.debug {
						c.logger.Printf("immediate %s failed: %v", env.endpoint, err)
					}
				} else {
					c.metrics.sent.Add(1)
				}
				continue
			}
			buffers[env.endpoint] = append(buffers[env.endpoint], env.payload)
			if len(buffers[env.endpoint]) >= c.cfg.BatchSize {
				c.flushEndpoint(context.Background(), env.endpoint, buffers)
			}
		case req, ok := <-c.flushCh:
			if !ok {
				continue
			}
			err := c.flushBuffers(req.ctx, buffers)
			if req.done != nil {
				req.done <- err
			}
		case <-ticker.C:
			_ = c.flushBuffers(context.Background(), buffers)
		case <-c.ctx.Done():
			c.flushBuffers(context.Background(), buffers)
			return
		}
	}
}

func (c *Client) flushBuffers(ctx context.Context, buffers map[string][]any) error {
	var firstErr error
	for endpoint := range buffers {
		if len(buffers[endpoint]) == 0 {
			continue
		}
		if err := c.flushEndpoint(ctx, endpoint, buffers); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Client) flushEndpoint(ctx context.Context, endpoint string, buffers map[string][]any) error {
	payloads := buffers[endpoint]
	if len(payloads) == 0 {
		return nil
	}
	body, err := json.Marshal(payloads)
	if err != nil {
		return fmt.Errorf("marshal batch: %w", err)
	}
	if err := c.postWithContext(ctx, endpoint, body, maxRetryAttempts); err != nil {
		c.metrics.failed.Add(uint64(len(payloads)))
		if c.debug {
			c.logger.Printf("batch %s failed: %v", endpoint, err)
		}
		return err
	}
	c.metrics.sent.Add(uint64(len(payloads)))
	buffers[endpoint] = buffers[endpoint][:0]
	return nil
}

func (c *Client) sendPayload(ctx context.Context, endpoint string, payload any) error {
	return c.sendPayloadAttempts(ctx, endpoint, payload, maxRetryAttempts)
}

func (c *Client) sendPayloadAttempts(ctx context.Context, endpoint string, payload any, attempts int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.postWithContext(ctx, endpoint, body, attempts)
}

func (c *Client) postWithContext(ctx context.Context, endpoint string, body []byte, attempts int) error {
	reqCtx, cancel := c.requestContext(ctx)
	defer cancel()
	return c.post(reqCtx, endpoint, body, attempts)
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte, attempts int) error {
	url := c.endpoint + endpoint
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.cfg.WriteKey)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("http %d %s", resp.StatusCode, resp.Status)
		}

		if attempt < attempts {
			delay := retryDelay(resp)
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	return lastErr
}

func retryDelay(resp *http.Response) time.Duration {
	if resp == nil {
		return 500 * time.Millisecond
	}
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 500 * time.Millisecond
	}
	if secs, err := time.ParseDuration(value + "s"); err == nil {
		return secs
	}
	if ts, err := time.Parse(time.RFC1123, value); err == nil {
		return time.Until(ts)
	}
	return 500 * time.Millisecond
}

func (c *Client) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	reqCtx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(c.ctx, cancel)
	return reqCtx, func() {
		cancel()
		stop()
	}
}

func normalizeEndpoint(raw string) string {
	if raw == "" {
		return defaultEndpoint
	}
	if strings.HasSuffix(raw, "/") {
		return raw
	}
	return raw + "/"
}

func defaultContextProps() Props {
	return Props{
		"library": map[string]any{
			"name":    "github.com/invisible-tools/raindrop-go/raindrop",
			"version": Version(),
		},
		"metadata": map[string]any{
			"goVersion": runtime.Version(),
			"os":        runtime.GOOS,
			"arch":      runtime.GOARCH,
		},
	}
}

func uuidNewString() string {
	return uuid.NewString()
}

func boolPtr(v bool) *bool {
	return &v
}
