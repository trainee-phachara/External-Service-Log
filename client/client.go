package logclient

import (
	"context"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/trainee-phachara/External-Service-Log/client/pb"
)

// LogSource identifies the application/service emitting a log entry.
type LogSource struct {
	AppName     string
	ServiceName string
}

// LogEntryInput is the data sent to external-service-log's IngestService.
type LogEntryInput struct {
	Source         LogSource
	TraceID        string
	Endpoint       string
	HTTPStatus     string
	Type           string
	Direction      string
	MetadataJSON   string
	RawPayloadJSON string
	PayloadJSON    string
}

// LogClient sends log entries to external-service-log.
type LogClient interface {
	// SendLog fires the log entry without waiting for the response.
	SendLog(entry LogEntryInput)
	Close() error
}

// Config holds options for creating a LogClient.
type Config struct {
	// Address is the gRPC server address (e.g. "localhost:50051").
	Address string
	// Timeout is the per-call deadline. Defaults to 5 seconds when zero.
	Timeout time.Duration
}

type grpcLogClient struct {
	conn    *grpc.ClientConn
	client  pb.IngestServiceClient
	timeout time.Duration
}

// New connects to the IngestService at cfg.Address.
func New(cfg Config) (LogClient, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}

	conn, err := grpc.NewClient(cfg.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &grpcLogClient{
		conn:    conn,
		client:  pb.NewIngestServiceClient(conn),
		timeout: cfg.Timeout,
	}, nil
}

func (c *grpcLogClient) SendLog(entry LogEntryInput) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()

		_, err := c.client.Ingest(ctx, &pb.IngestRequest{
			Source: &pb.LogSource{
				AppName:     entry.Source.AppName,
				ServiceName: entry.Source.ServiceName,
			},
			TraceId:        entry.TraceID,
			Endpoint:       entry.Endpoint,
			HttpStatus:     entry.HTTPStatus,
			Type:           entry.Type,
			Direction:      entry.Direction,
			MetadataJson:   entry.MetadataJSON,
			RawPayloadJson: entry.RawPayloadJSON,
			PayloadJson:    entry.PayloadJSON,
		})
		if err != nil {
			log.Printf("logclient: failed to send log: %v", err)
		}
	}()
}

func (c *grpcLogClient) Close() error {
	return c.conn.Close()
}
