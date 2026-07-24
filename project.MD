# External Service Log — คู่มือการทำงานทั้งระบบ

## Project นี้ทำอะไร

ในระบบที่มีหลาย service เช่น order-service, user-service, payment-service — แต่ละ service ต้องการบันทึก log ของตัวเอง (เช่น มีใครเรียก API ไหน ส่ง payload อะไรไป ได้ response กลับมาอะไร)

Project นี้สร้าง **ศูนย์กลางรับ log** แทนที่จะให้แต่ละ service บันทึก log เอง — service อื่นแค่ "ส่ง" log มาให้ แล้ว project นี้จะจัดการเก็บลง MongoDB ให้เอง

```
order-service ──┐
user-service  ──┼──► external-service-log ──► MongoDB
payment-service─┘
```

---

## โครงสร้าง Repository

```
Service-log/                          ← root ของ repo
├── go.work                           ← บอก Go ว่ามี 2 modules ใน repo นี้
│
├── client/                           ← Library ที่ service อื่นนำไปใช้ส่ง log
│   ├── go.mod
│   ├── client.go                     ← โค้ดหลักของ library
│   ├── client_test.go
│   └── pb/                           ← โค้ด gRPC ที่ generate มาจาก proto
│
├── services/
│   └── external-service-log/         ← ตัว server ที่รับ log และเก็บลง MongoDB
│       ├── go.mod
│       ├── Dockerfile
│       ├── cmd/server/main.go        ← จุดเริ่มต้นของ server
│       ├── proto/ingest.proto        ← นิยาม gRPC contract
│       └── internal/
│           ├── types/                ← โครงสร้างข้อมูล (struct)
│           ├── buffer/               ← เก็บ log ไว้ใน memory ชั่วคราว
│           ├── flusher/              ← บันทึก log จาก buffer ลง MongoDB เป็น batch
│           ├── ingest/               ← ตรวจสอบและประมวลผล log ที่รับเข้ามา
│           ├── logstore/             ← interface สำหรับ database
│           ├── mongostore/           ← implementation ของ logstore กับ MongoDB
│           ├── grpcserver/           ← รับ log ผ่าน gRPC
│           ├── grpc/pb/              ← โค้ด gRPC ที่ generate มาจาก proto
│           └── httpapi/              ← รับ log ผ่าน HTTP, ดึง log ผ่าน GET /logs
│
├── docker-compose.yml                ← รัน MongoDB + server ด้วยคำสั่งเดียว
└── .github/workflows/ci.yml         ← CI/CD pipeline รันอัตโนมัติบน GitHub
```

---

## ความสัมพันธ์ระหว่างส่วนต่างๆ

```
[service อื่น] → import client lib → SendLog() → gRPC → [grpcserver]
                                                              │
                                    HTTP POST /ingest → [httpapi]
                                                              │
                                                         [ingest]        ← validate + process
                                                              │
                                                         [buffer]        ← เก็บใน memory ก่อน
                                                              │
                                                         [flusher]       ← flush เป็น batch
                                                              │
                                                         [mongostore]    ← เขียนลง MongoDB
                                                              │
                                                         MongoDB (service_logs collection)
```

---

## อธิบายแต่ละส่วน

### 1. `client/` — Library สำหรับ service อื่น

Service อื่นที่อยากส่ง log มาให้ระบบนี้ ต้อง import library ตัวนี้

```go
import logclient "github.com/trainee-phachara/External-Serivce-Log/client"

// สร้าง client ครั้งเดียวตอน init
logger, err := logclient.New(logclient.Config{
    Address: "localhost:50051",  // address ของ external-service-log
})

// เรียกทุกครั้งที่อยาก log
logger.SendLog(logclient.LogEntryInput{
    Source:     logclient.LogSource{AppName: "order-service"},
    Endpoint:   "/api/orders",
    HTTPStatus: "200",
    Type:       "response",
    Direction:  "inbound",
})
```

`SendLog()` ทำงานแบบ **fire-and-forget** — ส่งแล้วไม่รอ response เพื่อไม่ให้กระทบ performance ของ service

การสื่อสารใช้ **gRPC** ซึ่งเร็วกว่า HTTP ปกติ เหมาะกับการส่งข้อมูลระหว่าง service

---

### 2. `proto/ingest.proto` — สัญญาระหว่าง client กับ server

ไฟล์ `.proto` คือการกำหนด "รูปแบบข้อมูล" ที่ client กับ server ต้องตกลงกันก่อน เหมือนสัญญาว่าจะส่งข้อมูลอะไรบ้าง

```proto
service IngestService {
  rpc Ingest (IngestRequest) returns (IngestResponse);
}

message IngestRequest {
  LogSource source    = 1;   // ชื่อ service ที่ส่งมา
  string trace_id     = 2;   // รหัสติดตาม request เดียวกันข้ามหลาย service
  string endpoint     = 3;   // path ที่ถูกเรียก เช่น /api/orders
  string http_status  = 4;   // status code เช่น 200, 404, 500
  string type         = 5;   // request | response | event
  string direction    = 6;   // inbound | outbound
  // ... payload fields
}
```

จากไฟล์ `.proto` นี้ใช้คำสั่ง `protoc` generate โค้ด Go ออกมาเป็นไฟล์ใน `pb/` โดยอัตโนมัติ ไม่ต้องเขียนเอง

---

### 3. `internal/types/` — โครงสร้างข้อมูล

นิยาม struct ที่ใช้ทั่วทั้งระบบ เช่น `LogEntry` ที่เก็บลง MongoDB:

```go
type LogEntry struct {
    Timestamp  time.Time   // เวลาที่รับ log
    Source     LogSource   // app_name, service_name
    TraceID    string      // รหัสติดตาม
    Endpoint   string      // path
    HTTPStatus string      // 200, 404, 500
    Type       LogType     // request | response | event
    Direction  LogDirection // inbound | outbound
    Metadata   map[string]interface{}
    RawPayload map[string]interface{}
    Payload    map[string]interface{}
}
```

log ทุกประเภทเก็บใน **collection เดียว** ชื่อ `service_logs` และแยกประเภทด้วย field `type`

---

### 4. `internal/buffer/` — In-Memory Buffer

แทนที่จะเขียนลง MongoDB ทันทีทุกครั้ง (ซึ่งช้าและสิ้นเปลืองการเชื่อมต่อ) ระบบจะ **เก็บ log ไว้ใน memory ก่อน** แล้วค่อยเขียนเป็น batch

```
log 1 ──► [buffer: log1, log2, log3, ...] ──► flush ──► MongoDB (เขียนครั้งเดียว 100 รายการ)
log 2 ──►
log 3 ──►
```

Buffer ใช้ `sync.Mutex` เพื่อให้หลาย goroutine (thread) เข้าถึงพร้อมกันได้อย่างปลอดภัย

---

### 5. `internal/flusher/` — Batch Flusher

ตัดสินใจว่า **เมื่อไหร่จะ flush** buffer ลง MongoDB มี 2 เงื่อนไข:

- **ทุก 5 วินาที** (timer-based) — flush ตามเวลา แม้มี log น้อย
- **ทุก 100 รายการ** (size-based) — flush ทันทีถ้า buffer เต็ม ไม่รอเวลา

```
timer (ทุก 5s)    ──► flush ──► MongoDB
buffer เต็ม 100   ──►
```

---

### 6. `internal/ingest/` — Validation Pipeline

ทุก log ที่เข้ามา (ทั้งจาก HTTP และ gRPC) ต้องผ่านการ validate ก่อน:

- `source.app_name` ต้องมี
- `type` ต้องเป็น `request`, `response`, หรือ `event`
- `direction` ต้องเป็น `inbound` หรือ `outbound`
- `http_status` ต้องมี

ถ้า validate ผ่าน → ส่งไปที่ buffer  
ถ้าไม่ผ่าน → ตอบกลับ error ทันที

---

### 7. `internal/grpcserver/` และ `internal/httpapi/`

รับ log เข้ามา 2 ช่องทาง:

| ช่องทาง | Port | ใช้กับ |
|---|---|---|
| gRPC | 50051 | service อื่นที่ import client lib |
| HTTP POST `/ingest` | 3000 | ทดสอบ หรือ service ที่ไม่ใช้ gRPC |
| HTTP GET `/logs` | 3000 | ดึง log กลับมาอ่าน |

---

### 8. `internal/mongostore/` — MongoDB Layer

เชื่อมต่อ MongoDB และมีหน้าที่ 2 อย่าง:

- `InsertLogs` — รับ batch ของ log แล้วเขียนลง collection `service_logs`
- `FindLogs` — query log ออกมาตาม filter (type, app_name, limit)

---

## วิธี Setup และ Run

### สิ่งที่ต้องติดตั้งก่อน

- [Go 1.24+](https://go.dev/dl/)
- [Docker Desktop](https://www.docker.com/products/docker-desktop/)

---

### วิธีที่ 1 — Docker Compose (แนะนำ)

รัน MongoDB และ server พร้อมกันด้วยคำสั่งเดียวจาก root ของ repo:

```sh
docker compose up --build
```

ระบบจะ:
1. สร้าง MongoDB container
2. build และรัน external-service-log
3. external-service-log รอให้ MongoDB พร้อมก่อน (healthcheck)

เมื่อพร้อมแล้ว:
- HTTP: `http://localhost:3000`
- gRPC: `localhost:50051`

---

### วิธีที่ 2 — รัน Local โดยไม่ใช้ Docker

ต้องมี MongoDB รันอยู่แล้ว (สามารถรันแค่ MongoDB ผ่าน Docker):

```sh
docker run -d -p 27017:27017 mongo:7
```

จากนั้นรัน server:

```sh
cd services/external-service-log
go run ./cmd/server
```

หรือกำหนด config เอง:

```sh
PORT=3000 GRPC_PORT=50051 MONGO_URI=mongodb://localhost:27017 go run ./cmd/server
```

---

### ทดสอบว่า server ทำงานได้

```sh
# ส่ง log ผ่าน HTTP
curl -X POST localhost:3000/ingest \
  -H "Content-Type: application/json" \
  -d '{
    "source": {"app_name": "order-service", "service_name": "order"},
    "trace_id": "trace-001",
    "endpoint": "/api/orders",
    "http_status": "200",
    "type": "response",
    "direction": "inbound"
  }'

# ดึง log กลับมา
curl "localhost:3000/logs?app=order-service&limit=10"
```

---

## วิธีที่ Service อื่นนำ Client Library ไปใช้

### ขั้นตอนที่ 1 — ติดตั้ง library

```sh
go get github.com/trainee-phachara/External-Serivce-Log/client@v0.1.0
```

### ขั้นตอนที่ 2 — ใช้ในโค้ด

```go
package main

import (
    "log"
    logclient "github.com/trainee-phachara/External-Serivce-Log/client"
)

func main() {
    // สร้าง client ครั้งเดียวตอน start
    logger, err := logclient.New(logclient.Config{
        Address: "localhost:50051",
    })
    if err != nil {
        log.Fatal(err)
    }
    defer logger.Close()

    // ส่ง log ได้เลย (ไม่บล็อก)
    logger.SendLog(logclient.LogEntryInput{
        Source:     logclient.LogSource{AppName: "order-service", ServiceName: "order"},
        TraceID:    "trace-001",
        Endpoint:   "/api/orders",
        HTTPStatus: "201",
        Type:       "response",
        Direction:  "inbound",
    })
}
```

`SendLog()` ส่งข้อมูลใน goroutine แยก ไม่ทำให้โค้ดหลักช้าลง ถ้า log server ไม่ตอบสนอง จะ log error ออกมาแต่ไม่ panic

---

## CI/CD Pipeline

ทุกครั้งที่ push ขึ้น `main` หรือเปิด PR เข้า `main` GitHub Actions จะรัน pipeline อัตโนมัติ

```
push to main
     │
     ├── Test client (parallel)
     │     ├── Build
     │     ├── Lint (golangci-lint)
     │     └── Test + Coverage gate >= 85%
     │
     └── Test external-service-log (parallel)
           ├── Build
           ├── Lint (golangci-lint)
           └── Test + Coverage gate >= 85%
```

ดูผลได้ที่ GitHub repo → tab **Actions**

### Coverage gate คืออะไร

ตัวนับเปอร์เซ็นต์ว่าโค้ดถูก test ครอบคลุมแค่ไหน ถ้าต่ำกว่า 85% pipeline จะ fail เพื่อบังคับให้เขียน test ครบ

---

## Environment Variables

| Variable | Default | คำอธิบาย |
|---|---|---|
| `PORT` | `3000` | HTTP port |
| `GRPC_PORT` | `50051` | gRPC port |
| `MONGO_URI` | `mongodb://localhost:27017` | MongoDB connection string |
| `MONGO_DB_NAME` | `service_logs` | ชื่อ database |
| `FLUSH_MAX_SIZE` | `100` | flush เมื่อ buffer มี log ครบกี่รายการ |
| `FLUSH_INTERVAL_MS` | `5000` | flush ทุกกี่ millisecond (5000 = 5 วินาที) |

---

## MongoDB Collection

log ทุกประเภทเก็บใน collection เดียว: `service_logs`

```json
{
  "timestamp": "2026-07-13T10:00:00Z",
  "source": {
    "app_name": "order-service",
    "service_name": "order"
  },
  "trace_id": "trace-001",
  "endpoint": "/api/orders",
  "http_status": "200",
  "type": "response",
  "direction": "inbound",
  "metadata": {},
  "raw_payload": {},
  "payload": {"order_id": 123}
}
```

กรอง log ตาม `type` ได้ผ่าน GET /logs?type=response

---

## Version Tags

library ใช้ Git tag บอก version เพื่อให้ `go get` ดึงเวอร์ชันที่ถูกต้องได้

```sh
# สร้าง tag ใหม่เมื่อมี breaking change หรือ feature ใหม่
git tag client/v0.2.0
git push origin client/v0.2.0
```

รูปแบบ tag ต้องขึ้นต้นด้วย `client/` เพราะ Go module proxy ใช้ prefix นี้หา module ย่อยใน monorepo

---

## การรัน Test

```sh
# test client library
cd client
go test ./...

# test server (ไม่รวม generated pb)
cd services/external-service-log
go test $(go list ./internal/... | grep -v '/pb')

# ดู coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out   # เปิดใน browser
```
