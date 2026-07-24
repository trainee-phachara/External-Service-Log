package logclient

import (
	"encoding/json"
	"fmt"

	"github.com/sirupsen/logrus"
)

// LogrusHook forwards logrus entries to external-service-log via a LogClient.
type LogrusHook struct {
	client LogClient
	source LogSource
}

// NewLogrusHook builds a hook that tags forwarded entries with source.
func NewLogrusHook(client LogClient, source LogSource) *LogrusHook {
	return &LogrusHook{client: client, source: source}
}

func (h *LogrusHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// Fire skips entries missing trace_id, endpoint, or http_status because the
// ingest service silently drops them.
func (h *LogrusHook) Fire(entry *logrus.Entry) error {
	traceID, ok := stringField(entry.Data, "trace_id")
	if !ok {
		return nil
	}
	endpoint, ok := stringField(entry.Data, "endpoint")
	if !ok {
		return nil
	}
	httpStatus, ok := stringField(entry.Data, "http_status")
	if !ok {
		return nil
	}

	source := h.source
	if svc, ok := stringField(entry.Data, "service_name"); ok {
		source.ServiceName = svc
	}

	input := LogEntryInput{
		Source:       source,
		TraceID:      traceID,
		Endpoint:     endpoint,
		HTTPStatus:   httpStatus,
		Type:         fieldOrDefault(entry.Data, "type", "event"),
		Direction:    fieldOrDefault(entry.Data, "direction", "outbound"),
		PayloadJSON:  marshalField(entry.Data, "payload"),
		MetadataJSON: buildMetadata(entry),
	}

	h.client.SendLog(input)
	return nil
}

func stringField(data logrus.Fields, key string) (string, bool) {
	v, ok := data[key]
	if !ok {
		return "", false
	}
	s := fmt.Sprintf("%v", v)
	if s == "" {
		return "", false
	}
	return s, true
}

func fieldOrDefault(data logrus.Fields, key, fallback string) string {
	if s, ok := stringField(data, key); ok {
		return s
	}
	return fallback
}

func marshalField(data logrus.Fields, key string) string {
	v, ok := data[key]
	if !ok {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

var consumedFields = map[string]struct{}{
	"trace_id":     {},
	"endpoint":     {},
	"http_status":  {},
	"type":         {},
	"direction":    {},
	"payload":      {},
	"service_name": {},
}

func buildMetadata(entry *logrus.Entry) string {
	metadata := map[string]any{
		"message": entry.Message,
		"level":   entry.Level.String(),
	}
	for k, v := range entry.Data {
		if _, consumed := consumedFields[k]; consumed {
			continue
		}
		metadata[k] = v
	}
	b, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(b)
}
