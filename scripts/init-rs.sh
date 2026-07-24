#!/bin/bash
# init-rs.sh — Initialize replica sets for config server and both shards.
# Idempotent: skips rs.initiate() if the replica set is already initialized.
# Runs once and exits. Depends on configsvr, shard1, shard2 being healthy.
set -e

wait_for_primary() {
  local host=$1
  local attempts=0
  echo "==> Waiting for primary at $host..."
  until mongosh --host "$host" --quiet --eval "db.hello().isWritablePrimary" 2>/dev/null | grep -q "true"; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 30 ]; then
      echo "ERROR: $host did not become primary after 60s" >&2
      exit 1
    fi
    sleep 2
  done
  echo "==> $host is primary"
}

echo "==> Initializing config server replica set (rs-config)..."
mongosh --host configsvr:27017 --eval '
  var s = rs.status();
  if (s.ok === 1) {
    print("already initialized, skipping");
  } else {
    rs.initiate({
      _id: "rs-config",
      configsvr: true,
      members: [{ _id: 0, host: "configsvr:27017" }]
    });
  }
'
wait_for_primary configsvr:27017

echo "==> Initializing shard1 replica set (rs-shard1)..."
mongosh --host shard1:27017 --eval '
  var s = rs.status();
  if (s.ok === 1) {
    print("already initialized, skipping");
  } else {
    rs.initiate({
      _id: "rs-shard1",
      members: [{ _id: 0, host: "shard1:27017" }]
    });
  }
'
wait_for_primary shard1:27017

echo "==> Initializing shard2 replica set (rs-shard2)..."
mongosh --host shard2:27017 --eval '
  var s = rs.status();
  if (s.ok === 1) {
    print("already initialized, skipping");
  } else {
    rs.initiate({
      _id: "rs-shard2",
      members: [{ _id: 0, host: "shard2:27017" }]
    });
  }
'
wait_for_primary shard2:27017

echo "==> All replica sets ready"
