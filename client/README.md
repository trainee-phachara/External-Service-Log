# logclient

Go client library for sending structured logs to the external-service-log gRPC server.

## Setup

### Using go.work (local development)

Add to your workspace's `go.work`:

```
use ./path/to/libs/client
```

### Using go get (published module)

```sh
go get github.com/trainee-phachara/External-Serivce-Log/client
```

## Usage

```go
import logclient "github.com/trainee-phachara/External-Serivce-Log/client"

// Create client — call once at startup
c, err := logclient.New(logclient.Config{
    Address: "localhost:50051", // gRPC address of external-service-log
    Timeout: 3 * time.Second,  // per-call timeout (default 5s)
})
if err != nil {
    log.Fatal(err)
}
defer c.Close()

// Send a log entry — fire-and-forget, non-blocking
c.SendLog(logclient.LogEntryInput{
    Source:         logclient.LogSource{AppName: "order-service", ServiceName: "order"},
    TraceID:        "trace-abc",
    Endpoint:       "/api/v1/orders",
    HTTPStatus:     "200",
    Type:           "response",
    Direction:      "inbound",
    MetadataJSON:   `{"user_id": "u-1"}`,
    RawPayloadJSON: `{"raw": true}`,
    PayloadJSON:    `{"id": 1}`,
})
```

### Log Types

| Type | Description |
|---|---|
| `request` | Incoming request before processing |
| `response` | Outgoing response after processing |
| `event` | Domain event (e.g. order.placed) |

### Direction

| Direction | Description |
|---|---|
| `inbound` | Request coming into the service |
| `outbound` | Request going out to another service |

## logrus hook

Already using [logrus](https://github.com/sirupsen/logrus)? Register the hook once and matching entries are forwarded to external-service-log automatically — no `SendLog` call at each log site.

```go
import (
    logclient "github.com/trainee-phachara/External-Serivce-Log/client"
    "github.com/sirupsen/logrus"
)

c, err := logclient.New(logclient.Config{Address: "localhost:50051"})
if err != nil {
    log.Fatal(err)
}
defer c.Close()

logrus.AddHook(logclient.NewLogrusHook(c, logclient.LogSource{
    AppName:     "order-service",
    ServiceName: "order",
}))

// Forwarded — carries the server-required trace_id, endpoint and http_status.
logrus.WithFields(logrus.Fields{
    "trace_id":    "trace-abc",
    "endpoint":    "/api/v1/orders",
    "http_status": "200",
    "type":        "response",              // optional, defaults to "event"
    "direction":   "inbound",               // optional, defaults to "outbound"
    "payload":     map[string]any{"id": 1}, // optional, becomes PayloadJSON
    "user_id":     "u-1",                   // extra fields go to metadata
}).Info("order created")

// Skipped — no trace_id/endpoint/http_status, so it is not forwarded.
logrus.Info("starting up")
```

The hook forwards only entries that carry `trace_id`, `endpoint` **and** `http_status`; anything missing one of those is ignored, so plain application logs are left alone. The message and level, plus any extra fields, are stored as the entry's metadata.
