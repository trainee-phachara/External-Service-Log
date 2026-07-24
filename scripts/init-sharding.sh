#!/bin/bash
# init-sharding.sh — Add shards to mongos, enable sharding on the database,
# and create + shard the service_logs time-series collection.
# Runs once and exits. Depends on mongos being healthy.
set -e

echo "==> Adding shards to mongos..."
mongosh --host mongos:27017 --eval '
sh.addShard("rs-shard1/shard1:27017")
sh.addShard("rs-shard2/shard2:27017")
'

echo "==> Waiting for shards to be recognized..."
attempts=0
until mongosh --host mongos:27017 --quiet --eval \
  'quit(db.adminCommand({listShards:1}).shards.length >= 2 ? 0 : 1)' 2>/dev/null; do
  attempts=$((attempts + 1))
  if [ "$attempts" -ge 30 ]; then
    echo "ERROR: shards not recognized after 60s" >&2
    exit 1
  fi
  sleep 2
done

echo "==> Enabling sharding on database service_logs..."
mongosh --host mongos:27017 --eval '
sh.enableSharding("service_logs")
'

echo "==> Creating service_logs time-series collection (if not exists)..."
mongosh --host mongos:27017 --eval '
const db = db.getSiblingDB("service_logs");
const names = db.getCollectionNames();
if (!names.includes("service_logs")) {
  db.createCollection("service_logs", {
    timeseries: {
      timeField:   "timestamp",
      metaField:   "source",
      granularity: "seconds"
    },
    expireAfterSeconds: 3456000
  });
  print("Created service_logs collection");
} else {
  print("service_logs already exists — skipping create");
}
'

echo "==> Sharding service_logs collection on { source: hashed }..."
mongosh --host mongos:27017 --eval '
  var already = db.getSiblingDB("config").collections.findOne({ _id: "service_logs.service_logs" });
  if (!already) {
    sh.shardCollection("service_logs.service_logs", { source: "hashed" });
    print("shardCollection done");
  } else {
    print("already sharded, skipping");
  }
'

echo "==> Sharding setup complete"
