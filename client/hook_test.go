package logclient

import (
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
)

type fakeLogClient struct {
	mu      sync.Mutex
	entries []LogEntryInput
}

func (c *fakeLogClient) SendLog(entry LogEntryInput) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, entry)
}

func (c *fakeLogClient) Close() error { return nil }

func (c *fakeLogClient) recorded() []LogEntryInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]LogEntryInput(nil), c.entries...)
}

func newHookLogger(hook *LogrusHook) *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	logger.AddHook(hook)
	return logger
}

var testSource = LogSource{AppName: "order-service", ServiceName: "order"}

func TestFire_ForwardsEntryWithDefaults(t *testing.T) {
	client := &fakeLogClient{}
	logger := newHookLogger(NewLogrusHook(client, testSource))

	logger.WithFields(logrus.Fields{
		"trace_id":    "trace-1",
		"endpoint":    "/orders",
		"http_status": 200,
	}).Info("created order")

	recorded := client.recorded()
	if len(recorded) != 1 {
		t.Fatalf("SendLog called %d times, want 1", len(recorded))
	}
	got := recorded[0]

	if got.Source != testSource {
		t.Errorf("Source = %+v, want %+v", got.Source, testSource)
	}
	if got.TraceID != "trace-1" {
		t.Errorf("TraceID = %q, want %q", got.TraceID, "trace-1")
	}
	if got.Endpoint != "/orders" {
		t.Errorf("Endpoint = %q, want %q", got.Endpoint, "/orders")
	}
	if got.HTTPStatus != "200" {
		t.Errorf("HTTPStatus = %q, want %q", got.HTTPStatus, "200")
	}
	if got.Type != "event" {
		t.Errorf("Type = %q, want default %q", got.Type, "event")
	}
	if got.Direction != "outbound" {
		t.Errorf("Direction = %q, want default %q", got.Direction, "outbound")
	}

	metadata := unmarshalMetadata(t, got.MetadataJSON)
	if metadata["message"] != "created order" {
		t.Errorf("metadata message = %v, want %q", metadata["message"], "created order")
	}
	if metadata["level"] != "info" {
		t.Errorf("metadata level = %v, want %q", metadata["level"], "info")
	}
}

func TestFire_SkipsEntryMissingHTTPStatus(t *testing.T) {
	client := &fakeLogClient{}
	logger := newHookLogger(NewLogrusHook(client, testSource))

	logger.WithFields(logrus.Fields{
		"trace_id": "trace-1",
		"endpoint": "/orders",
	}).Info("missing status")

	if recorded := client.recorded(); len(recorded) != 0 {
		t.Fatalf("SendLog called %d times, want 0", len(recorded))
	}
}

func TestFire_ExplicitFieldsOverrideDefaults(t *testing.T) {
	client := &fakeLogClient{}
	logger := newHookLogger(NewLogrusHook(client, testSource))

	logger.WithFields(logrus.Fields{
		"trace_id":    "trace-2",
		"endpoint":    "/payments",
		"http_status": "201",
		"type":        "response",
		"direction":   "inbound",
		"payload":     map[string]any{"id": 7},
		"user_id":     "u-42",
	}).Warn("payment done")

	recorded := client.recorded()
	if len(recorded) != 1 {
		t.Fatalf("SendLog called %d times, want 1", len(recorded))
	}
	got := recorded[0]

	if got.Type != "response" {
		t.Errorf("Type = %q, want %q", got.Type, "response")
	}
	if got.Direction != "inbound" {
		t.Errorf("Direction = %q, want %q", got.Direction, "inbound")
	}

	payload := unmarshalMetadata(t, got.PayloadJSON)
	if payload["id"] != float64(7) {
		t.Errorf("payload id = %v, want 7", payload["id"])
	}

	metadata := unmarshalMetadata(t, got.MetadataJSON)
	for _, consumed := range []string{"trace_id", "endpoint", "http_status", "type", "direction", "payload"} {
		if _, leaked := metadata[consumed]; leaked {
			t.Errorf("consumed field %q leaked into metadata", consumed)
		}
	}
	if metadata["user_id"] != "u-42" {
		t.Errorf("metadata user_id = %v, want %q", metadata["user_id"], "u-42")
	}
}

func TestFire_ServiceNameOverridesDefault(t *testing.T) {
	client := &fakeLogClient{}
	logger := newHookLogger(NewLogrusHook(client, testSource))

	logger.WithFields(logrus.Fields{
		"trace_id":     "trace-3",
		"endpoint":     "/users/1",
		"http_status":  "200",
		"service_name": "user-service",
	}).Info("validated user")

	recorded := client.recorded()
	if len(recorded) != 1 {
		t.Fatalf("SendLog called %d times, want 1", len(recorded))
	}
	got := recorded[0]

	if got.Source.AppName != testSource.AppName {
		t.Errorf("AppName = %q, want %q", got.Source.AppName, testSource.AppName)
	}
	if got.Source.ServiceName != "user-service" {
		t.Errorf("ServiceName = %q, want %q", got.Source.ServiceName, "user-service")
	}
}

func TestFire_DefaultServiceNameKeptWhenNoField(t *testing.T) {
	client := &fakeLogClient{}
	logger := newHookLogger(NewLogrusHook(client, testSource))

	logger.WithFields(logrus.Fields{
		"trace_id":    "trace-4",
		"endpoint":    "/orders",
		"http_status": "201",
	}).Info("order created")

	recorded := client.recorded()
	if len(recorded) != 1 {
		t.Fatalf("SendLog called %d times, want 1", len(recorded))
	}
	if recorded[0].Source != testSource {
		t.Errorf("Source = %+v, want %+v", recorded[0].Source, testSource)
	}
}

func TestFire_ServiceNameNotLeakedToMetadata(t *testing.T) {
	client := &fakeLogClient{}
	logger := newHookLogger(NewLogrusHook(client, testSource))

	logger.WithFields(logrus.Fields{
		"trace_id":     "trace-5",
		"endpoint":     "/users/1",
		"http_status":  "200",
		"service_name": "user-service",
	}).Info("ok")

	recorded := client.recorded()
	if len(recorded) != 1 {
		t.Fatalf("SendLog called %d times, want 1", len(recorded))
	}
	metadata := unmarshalMetadata(t, recorded[0].MetadataJSON)
	if _, leaked := metadata["service_name"]; leaked {
		t.Error("service_name leaked into metadata, want it consumed")
	}
}

func unmarshalMetadata(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", s, err)
	}
	return m
}
