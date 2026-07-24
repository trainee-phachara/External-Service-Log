package archiver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"external-service-log/internal/types"
)

// newTestDB spins up a MongoDB container and returns a test database with the
// hot time-series collection already created.
func newTestDB(t *testing.T) *mongo.Database {
	t.Helper()
	ctx := context.Background()

	container, err := mongodb.Run(ctx, "mongo:7")
	if err != nil {
		t.Fatalf("start mongodb container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("archiver_test")

	// Create the hot collection as a time-series collection (same shape as prod).
	if err := db.RunCommand(ctx, bson.D{
		{Key: "create", Value: types.CollectionName},
		{Key: "timeseries", Value: bson.D{
			{Key: "timeField", Value: "timestamp"},
			{Key: "metaField", Value: "source"},
			{Key: "granularity", Value: "seconds"},
		}},
	}).Err(); err != nil {
		t.Fatalf("create hot collection: %v", err)
	}

	return db
}

// insertLog adds a single log entry at timestamp ts into the hot collection.
func insertLog(t *testing.T, db *mongo.Database, ts time.Time, traceID string) {
	t.Helper()
	entry := types.LogEntry{
		Timestamp:  ts,
		Source:     types.LogSource{AppName: "test-service", ServiceName: "test"},
		TraceID:    traceID,
		Endpoint:   "/test",
		HTTPStatus: "200",
		Type:       types.LogTypeResponse,
		Direction:  types.LogDirectionInbound,
		Metadata:   map[string]interface{}{},
		RawPayload: map[string]interface{}{},
		Payload:    map[string]interface{}{},
	}
	if _, err := db.Collection(types.CollectionName).InsertOne(context.Background(), entry); err != nil {
		t.Fatalf("insert test log: %v", err)
	}
}

func TestRun_NoLogs_DoesNothing(t *testing.T) {
	db := newTestDB(t)
	a := New(db)

	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	names, err := db.ListCollectionNames(context.Background(), bson.D{})
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "service_logs_archive_") {
			t.Errorf("unexpected archive collection created: %s", name)
		}
	}
}

func TestRun_ArchivesOldLogs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	oldTime := time.Now().UTC().AddDate(0, 0, -31)
	insertLog(t, db, oldTime, "old-trace")
	insertLog(t, db, time.Now().UTC(), "new-trace") // should NOT be archived

	a := &Archiver{db: db, now: time.Now}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	archiveName := archiveCollectionName(oldTime)
	count, err := db.Collection(archiveName).CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count archive: %v", err)
	}
	if count != 1 {
		t.Errorf("archive count = %d, want 1", count)
	}

	dayKey := oldTime.Truncate(24 * time.Hour).Format("2006-01-02")
	var state archiveState
	if err := db.Collection(stateCollection).FindOne(ctx, bson.D{{Key: "_id", Value: dayKey}}).Decode(&state); err != nil {
		t.Fatalf("find archive_state: %v", err)
	}
	if state.Status != "done" {
		t.Errorf("status = %q, want done", state.Status)
	}
	if state.Count != 1 {
		t.Errorf("count in state = %d, want 1", state.Count)
	}
}

func TestRun_RecentLogsStayInHot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Insert only a recent log (15 days ago) — should not be archived.
	insertLog(t, db, time.Now().UTC().AddDate(0, 0, -15), "recent-trace")

	a := &Archiver{db: db, now: time.Now}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	names, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "service_logs_archive_") {
			t.Errorf("recent log should not be archived, but found: %s", name)
		}
	}
}

func TestRun_IsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	oldTime := time.Now().UTC().AddDate(0, 0, -31)
	insertLog(t, db, oldTime, "old-trace")

	a := &Archiver{db: db, now: time.Now}

	if err := a.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	// No duplicates — archive must still have exactly 1 document.
	archiveName := archiveCollectionName(oldTime)
	count, err := db.Collection(archiveName).CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count archive: %v", err)
	}
	if count != 1 {
		t.Errorf("archive count after 2 runs = %d, want 1 (idempotency broken)", count)
	}
}

func TestRun_SkipsDoneDays(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	oldTime := time.Now().UTC().AddDate(0, 0, -31)
	dayKey := oldTime.UTC().Truncate(24 * time.Hour).Format("2006-01-02")
	insertLog(t, db, oldTime, "old-trace")

	// Pre-mark the day as done so the archiver should skip it.
	if _, err := db.Collection(stateCollection).InsertOne(ctx, archiveState{
		ID: dayKey, Status: "done", Count: 1, ArchivedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("pre-mark done: %v", err)
	}

	a := &Archiver{db: db, now: time.Now}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Archive collection should not have been created.
	archiveName := archiveCollectionName(oldTime)
	count, err := db.Collection(archiveName).CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count archive: %v", err)
	}
	if count != 0 {
		t.Errorf("done day should be skipped, but archive has %d documents", count)
	}
}

func TestEnsureArchiveCollection_IsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := &Archiver{db: db, now: time.Now}

	name := "service_logs_archive_2026_07"
	if err := a.ensureArchiveCollection(ctx, name); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := a.ensureArchiveCollection(ctx, name); err != nil {
		t.Fatalf("second ensure (should be no-op): %v", err)
	}
}
