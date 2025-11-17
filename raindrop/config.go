package raindrop

import (
	"errors"
	"net/http"
	"time"
)

const (
	defaultEndpoint             = "https://api.raindrop.ai/v1/"
	defaultBatchSize            = 10
	defaultMaxQueueSize         = 10_000
	defaultFlushInterval        = time.Second
	defaultPartialFlushInterval = 2 * time.Second
	maxPayloadBytes             = 1 << 20 // 1 MB

	maxRetryAttempts = 3
)

var errMissingWriteKey = errors.New("raindrop: write key is required")

// Config contains the knobs for client behavior.
type Config struct {
	WriteKey             string
	Endpoint             string
	FlushInterval        time.Duration
	BatchSize            int
	MaxQueueSize         int
	HTTPClient           *http.Client
	Debug                bool
	PartialFlushInterval time.Duration
	Tracing              TracingConfig
}

// TracingConfig determines how the Traceloop adapter behaves.
type TracingConfig struct {
	Enabled               bool
	APIKey                string
	BaseURL               string // optional custom OTLP endpoint; defaults to <Endpoint>/v1/traces
	ServiceName           string
	AssociationProperties map[string]string
}

func (c *Config) applyDefaults() error {
	if c.WriteKey == "" {
		return errMissingWriteKey
	}

	if c.Endpoint == "" {
		c.Endpoint = defaultEndpoint
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = defaultFlushInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.MaxQueueSize <= 0 {
		c.MaxQueueSize = defaultMaxQueueSize
	}
	if c.PartialFlushInterval <= 0 {
		c.PartialFlushInterval = defaultPartialFlushInterval
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	if c.Tracing.Enabled || c.Tracing.APIKey != "" {
		c.Tracing.Enabled = true
		if c.Tracing.APIKey == "" {
			c.Tracing.APIKey = c.WriteKey
		}
	}

	return nil
}
