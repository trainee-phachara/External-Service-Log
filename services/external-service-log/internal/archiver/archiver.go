package archiver

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"external-service-log/internal/types"
)

const (
	hotCollection     = types.CollectionName // "service_logs"
	stateCollection   = "archive_state"
	archiveDays       = 30
	warnThresholdDays = 35
	batchSize         = 5000
)

// archiveState is one document in archive_state — tracks whether a calendar
// day has been fully archived.
type archiveState struct {
	ID         string    `bson:"_id"`
	Status     string    `bson:"status"`
	Count      int64     `bson:"count"`
	ArchivedAt time.Time `bson:"archivedAt"`
	DurationMs int64     `bson:"durationMs"`
}

// Archiver copies log entries older than archiveDays from the hot collection
// into monthly archive collections and records progress in archive_state.
type Archiver struct {
	db  *mongo.Database
	now func() time.Time
}

// New returns an Archiver backed by db.
func New(db *mongo.Database) *Archiver {
	return &Archiver{db: db, now: time.Now}
}

// Run performs one archiver pass: finds all un-archived days older than
// archiveDays and copies them to the appropriate monthly archive collection.
func (a *Archiver) Run(ctx context.Context) error {
	pending, err := a.pendingDays(ctx)
	if err != nil {
		return fmt.Errorf("find pending days: %w", err)
	}

	if len(pending) == 0 {
		log.Println("archiver: no pending days")
		return nil
	}

	// Alert when oldest pending day is close to the 40-day TTL boundary.
	ageDays := a.now().UTC().Sub(pending[0]).Hours() / 24
	log.Printf("archiver: oldest pending day %s (%.0f days old)", pending[0].Format("2006-01-02"), ageDays)
	if ageDays > warnThresholdDays {
		log.Printf("archiver: WARNING oldest un-archived day is %.0f days old — approaching 40-day TTL", ageDays)
	}

	processed := 0
	for _, day := range pending {
		if err := a.archiveDay(ctx, day); err != nil {
			log.Printf("archiver: day %s failed: %v", day.Format("2006-01-02"), err)
			continue
		}
		processed++
	}
	log.Printf("archiver: %d/%d days processed", processed, len(pending))
	return nil
}

// pendingDays returns all calendar days (UTC) that are older than archiveDays
// and do not have a "done" entry in archive_state, sorted oldest first.
func (a *Archiver) pendingDays(ctx context.Context) ([]time.Time, error) {
	cutoff := a.now().UTC().Truncate(24 * time.Hour).AddDate(0, 0, -archiveDays)

	// Find the earliest log in the hot collection.
	type tsDoc struct {
		Timestamp time.Time `bson:"timestamp"`
	}
	var earliest tsDoc
	err := a.db.Collection(hotCollection).FindOne(
		ctx,
		bson.D{},
		options.FindOne().
			SetSort(bson.D{{Key: "timestamp", Value: 1}}).
			SetProjection(bson.D{{Key: "timestamp", Value: 1}}),
	).Decode(&earliest)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find earliest log: %w", err)
	}

	startDay := earliest.Timestamp.UTC().Truncate(24 * time.Hour)

	// Collect already-done days from archive_state.
	cursor, err := a.db.Collection(stateCollection).Find(
		ctx,
		bson.D{{Key: "status", Value: "done"}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("read archive state: %w", err)
	}
	defer cursor.Close(ctx)

	done := map[string]bool{}
	for cursor.Next(ctx) {
		var s archiveState
		if err := cursor.Decode(&s); err != nil {
			continue
		}
		done[s.ID] = true
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate archive state: %w", err)
	}

	var pending []time.Time
	for d := startDay; d.Before(cutoff); d = d.AddDate(0, 0, 1) {
		if !done[d.Format("2006-01-02")] {
			pending = append(pending, d)
		}
	}
	return pending, nil
}

// archiveDay copies all hot-collection documents for one UTC calendar day into
// the appropriate monthly archive collection, verifies the count, and marks
// the day done in archive_state.
func (a *Archiver) archiveDay(ctx context.Context, day time.Time) error {
	start := a.now()
	dayStart := day.UTC()
	dayEnd := dayStart.AddDate(0, 0, 1)
	dayKey := dayStart.Format("2006-01-02")

	collName := archiveCollectionName(dayStart)
	if err := a.ensureArchiveCollection(ctx, collName); err != nil {
		return fmt.Errorf("ensure collection %s: %w", collName, err)
	}

	archiveColl := a.db.Collection(collName)
	timeRange := bson.D{{Key: "timestamp", Value: bson.D{
		{Key: "$gte", Value: dayStart},
		{Key: "$lt", Value: dayEnd},
	}}}

	// Idempotency: remove any partial copy from a previous failed run.
	if _, err := archiveColl.DeleteMany(ctx, timeRange); err != nil {
		return fmt.Errorf("idempotency delete: %w", err)
	}

	hotCount, err := a.db.Collection(hotCollection).CountDocuments(ctx, timeRange)
	if err != nil {
		return fmt.Errorf("count hot: %w", err)
	}
	if hotCount == 0 {
		return a.markDone(ctx, dayKey, 0, a.now().Sub(start))
	}

	// Stream hot documents and insert into archive in batches.
	cursor, err := a.db.Collection(hotCollection).Find(ctx, timeRange)
	if err != nil {
		return fmt.Errorf("cursor hot: %w", err)
	}
	defer cursor.Close(ctx)

	var batch []types.LogEntry
	var copied int64
	for cursor.Next(ctx) {
		var entry types.LogEntry
		if err := cursor.Decode(&entry); err != nil {
			return fmt.Errorf("decode entry: %w", err)
		}
		batch = append(batch, entry)
		if len(batch) >= batchSize {
			if err := a.insertBatch(ctx, archiveColl, batch); err != nil {
				return fmt.Errorf("insert batch: %w", err)
			}
			copied += int64(len(batch))
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := a.insertBatch(ctx, archiveColl, batch); err != nil {
			return fmt.Errorf("insert batch: %w", err)
		}
		copied += int64(len(batch))
	}
	if err := cursor.Err(); err != nil {
		return fmt.Errorf("cursor error: %w", err)
	}

	// Verify: both sides must agree before marking done.
	archiveCount, err := archiveColl.CountDocuments(ctx, timeRange)
	if err != nil {
		return fmt.Errorf("count archive: %w", err)
	}
	if archiveCount != hotCount {
		return fmt.Errorf("count mismatch for %s: hot=%d archive=%d", dayKey, hotCount, archiveCount)
	}

	log.Printf("archiver: day %s — archived %d documents", dayKey, copied)
	return a.markDone(ctx, dayKey, copied, a.now().Sub(start))
}

// archiveCollectionName returns the monthly archive collection name for time t.
func archiveCollectionName(t time.Time) string {
	return fmt.Sprintf("service_logs_archive_%d_%02d", t.Year(), int(t.Month()))
}

// ensureArchiveCollection creates the monthly time-series archive collection
// with zstd compression if it does not already exist.
func (a *Archiver) ensureArchiveCollection(ctx context.Context, name string) error {
	names, err := a.db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: name}})
	if err != nil {
		return err
	}
	if len(names) > 0 {
		return nil
	}

	return a.db.RunCommand(ctx, bson.D{
		{Key: "create", Value: name},
		{Key: "timeseries", Value: bson.D{
			{Key: "timeField", Value: "timestamp"},
			{Key: "metaField", Value: "source"},
			{Key: "granularity", Value: "seconds"},
		}},
		{Key: "storageEngine", Value: bson.D{
			{Key: "wiredTiger", Value: bson.D{
				{Key: "configString", Value: "block_compressor=zstd"},
			}},
		}},
	}).Err()
}

func (a *Archiver) insertBatch(ctx context.Context, coll *mongo.Collection, entries []types.LogEntry) error {
	docs := make([]interface{}, len(entries))
	for i, e := range entries {
		docs[i] = e
	}
	_, err := coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
	return err
}

func (a *Archiver) markDone(ctx context.Context, dayKey string, count int64, duration time.Duration) error {
	state := archiveState{
		ID:         dayKey,
		Status:     "done",
		Count:      count,
		ArchivedAt: a.now().UTC(),
		DurationMs: duration.Milliseconds(),
	}
	_, err := a.db.Collection(stateCollection).ReplaceOne(
		ctx,
		bson.D{{Key: "_id", Value: dayKey}},
		state,
		options.Replace().SetUpsert(true),
	)
	return err
}
