package mongostore

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"external-service-log/internal/logstore"
	"external-service-log/internal/types"
)

// ttlSeconds is how long documents remain in the hot collection before MongoDB
// expires them (40 days). The extra 10 days beyond the 30-day archive window
// gives the archiver time to copy data before TTL deletes it.
const ttlSeconds = 60 * 60 * 24 * 40


// Store persists buffered logs into MongoDB time-series collections.
type Store struct {
	client *mongo.Client
	db     *mongo.Database
}

// Connect opens a MongoDB connection, selects dbName, and ensures the
// api_logs, event_logs, and error_logs time-series collections exist.
func Connect(ctx context.Context, uri, dbName string) (*Store, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("connect to mongo: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("ping mongo: %w", err)
	}

	db := client.Database(dbName)
	if err := ensureTimeSeriesCollections(ctx, db); err != nil {
		return nil, err
	}

	return &Store{client: client, db: db}, nil
}

func ensureTimeSeriesCollections(ctx context.Context, db *mongo.Database) error {
	existing, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return fmt.Errorf("list collections: %w", err)
	}

	for _, name := range existing {
		if name == types.CollectionName {
			return nil
		}
	}

	timeSeriesOpts := options.TimeSeries().
		SetTimeField("timestamp").
		SetMetaField("source").
		SetGranularity("seconds")
	opts := options.CreateCollection().
		SetTimeSeriesOptions(timeSeriesOpts).
		SetExpireAfterSeconds(ttlSeconds)

	if err := db.CreateCollection(ctx, types.CollectionName, opts); err != nil {
		return fmt.Errorf("create collection %s: %w", types.CollectionName, err)
	}

	return nil
}

// FindLogs returns recent log entries, newest first.
func (s *Store) FindLogs(ctx context.Context, filter logstore.FindLogsFilter) ([]types.LogEntry, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	query := bson.D{}
	if filter.AppName != "" {
		query = append(query, bson.E{Key: "source.app_name", Value: filter.AppName})
	}
	if filter.Type != "" {
		query = append(query, bson.E{Key: "type", Value: string(filter.Type)})
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "timestamp", Value: -1}}).
		SetLimit(limit)

	cursor, err := s.db.Collection(types.CollectionName).Find(ctx, query, opts)
	if err != nil {
		return nil, fmt.Errorf("find logs: %w", err)
	}
	defer cursor.Close(ctx)

	var entries []types.LogEntry
	if err := cursor.All(ctx, &entries); err != nil {
		return nil, fmt.Errorf("decode logs: %w", err)
	}
	if entries == nil {
		entries = []types.LogEntry{}
	}
	return entries, nil
}

// InsertLogs inserts all buffered logs into the service_logs collection.
func (s *Store) InsertLogs(ctx context.Context, logs []types.BufferedLog) error {
	entries := make([]interface{}, len(logs))
	for i, log := range logs {
		entries[i] = log.Entry
	}

	if _, err := s.db.Collection(types.CollectionName).InsertMany(ctx, entries); err != nil {
		return fmt.Errorf("insert into %s: %w", types.CollectionName, err)
	}

	return nil
}

// Close disconnects the underlying MongoDB client.
func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}
