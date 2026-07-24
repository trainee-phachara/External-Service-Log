package logclient

import (
	"context"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/trainee-phachara/External-Service-Log/client/pb"
)

type fakeIngestServer struct {
	pb.UnimplementedIngestServiceServer
	handle func(context.Context, *pb.IngestRequest) (*pb.IngestResponse, error)
}

func (s *fakeIngestServer) Ingest(ctx context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
	return s.handle(ctx, req)
}

func startTestServer(t *testing.T, fake *fakeIngestServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	pb.RegisterIngestServiceServer(server, fake)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return lis.Addr().String()
}

func validEntry(modify func(*LogEntryInput)) LogEntryInput {
	e := LogEntryInput{
		Source:         LogSource{AppName: "order-service", ServiceName: "order"},
		TraceID:        "trace-1",
		Endpoint:       "/orders",
		HTTPStatus:     "200",
		Type:           "response",
		Direction:      "inbound",
		MetadataJSON:   `{"method":"GET"}`,
		RawPayloadJSON: "{}",
		PayloadJSON:    `{"id":1}`,
	}
	if modify != nil {
		modify(&e)
	}
	return e
}

func newTestClient(t *testing.T, addr string) LogClient {
	t.Helper()
	c, err := New(Config{Address: addr, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSendLog_SendsAllFieldsToIngestService(t *testing.T) {
	received := make(chan *pb.IngestRequest, 1)
	addr := startTestServer(t, &fakeIngestServer{
		handle: func(_ context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
			received <- req
			return &pb.IngestResponse{Accepted: true}, nil
		},
	})

	c := newTestClient(t, addr)
	c.SendLog(validEntry(func(e *LogEntryInput) { e.TraceID = "trace-abc" }))

	req := <-received
	if req.TraceId != "trace-abc" {
		t.Errorf("TraceId = %q, want %q", req.TraceId, "trace-abc")
	}
	if req.Endpoint != "/orders" {
		t.Errorf("Endpoint = %q, want %q", req.Endpoint, "/orders")
	}
	if req.HttpStatus != "200" {
		t.Errorf("HttpStatus = %q, want %q", req.HttpStatus, "200")
	}
	if req.Source.GetAppName() != "order-service" || req.Source.GetServiceName() != "order" {
		t.Errorf("Source = %+v, want {order-service order}", req.Source)
	}
}

func TestSendLog_LogsErrorWhenServerFails(t *testing.T) {
	addr := startTestServer(t, &fakeIngestServer{
		handle: func(_ context.Context, _ *pb.IngestRequest) (*pb.IngestResponse, error) {
			return nil, status.Error(codes.Internal, "boom")
		},
	})

	c := newTestClient(t, addr)

	var buf strings.Builder
	var mu sync.Mutex
	done := make(chan struct{})

	orig := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		n, err := buf.Write(p)
		select {
		case <-done:
		default:
			close(done)
		}
		return n, err
	}))
	t.Cleanup(func() { log.SetOutput(orig) })

	c.SendLog(validEntry(nil))
	<-done

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), "logclient: failed to send log") {
		t.Errorf("log output = %q, want failure message", buf.String())
	}
}

func TestSendLog_RespectsTimeout(t *testing.T) {
	addr := startTestServer(t, &fakeIngestServer{
		handle: func(ctx context.Context, _ *pb.IngestRequest) (*pb.IngestResponse, error) {
			<-ctx.Done()
			return nil, status.Error(codes.DeadlineExceeded, "timeout")
		},
	})

	done := make(chan struct{})
	orig := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		select {
		case <-done:
		default:
			close(done)
		}
		return len(p), nil
	}))
	t.Cleanup(func() { log.SetOutput(orig) })

	c, _ := New(Config{Address: addr, Timeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = c.Close() })
	c.SendLog(validEntry(nil))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SendLog did not time out within 2s")
	}
}

func TestClose_DoesNotError(t *testing.T) {
	addr := startTestServer(t, &fakeIngestServer{
		handle: func(_ context.Context, _ *pb.IngestRequest) (*pb.IngestResponse, error) {
			return &pb.IngestResponse{Accepted: true}, nil
		},
	})
	c, err := New(Config{Address: addr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
