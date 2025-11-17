package raindrop

import "testing"

func TestConfigApplyDefaults(t *testing.T) {
	cfg := Config{
		WriteKey: "test",
	}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults error: %v", err)
	}
	if cfg.Endpoint != defaultEndpoint {
		t.Fatalf("expected default endpoint, got %s", cfg.Endpoint)
	}
	if cfg.BatchSize != defaultBatchSize {
		t.Fatalf("expected default batch size %d, got %d", defaultBatchSize, cfg.BatchSize)
	}
	if cfg.PartialFlushInterval != defaultPartialFlushInterval {
		t.Fatalf("expected default partial flush interval %s, got %s", defaultPartialFlushInterval, cfg.PartialFlushInterval)
	}
	if cfg.HTTPClient == nil {
		t.Fatalf("expected default HTTP client")
	}
}

func TestConfigRequiresWriteKey(t *testing.T) {
	cfg := Config{}
	if err := cfg.applyDefaults(); err == nil {
		t.Fatalf("expected error when write key missing")
	}
}
