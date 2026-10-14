# BACnet Explorer & Historian — Specification (v1 draft)

## 1. Summary

A single Go service (`cmd/explorer`) that discovers BACnet/IP and routed (MS/TP etc.) devices, lets a user browse devices → objects → properties in a web UI, and lets them mark points for historizing. The service polls those points on a schedule and writes samples to TimescaleDB (Postgres). The web UI is embedded in the binary.

**The app is strictly read-only on the BACnet network.** It never changes device state.

## 2. Goals / Non-goals

**Goals (v1)**
- Discover devices on the local BACnet/IP subnet, behind BACnet routers (DNET/DADR), and on remote IP subnets via BBMD foreign-device registration.
- Browse device and object metadata and live property values.
- Configure historizing per point: property, poll interval, and optional deadband.
- Poll efficiently: batch reads with ReadPropertyMultiple, respect device limits, back off from devices that are offline.
- Store samples in TimescaleDB, then chart and export them.
- Provide diagnostics: decoded packet log, device comms health, and the BBMD/FDT view.

**Non-goals (v1)**
- Any write service. The transport layer blocks these outright (see §5.1): WriteProperty, WritePropertyMultiple, CreateObject, DeleteObject, ReinitializeDevice, DeviceCommunicationControl, Add/RemoveListElement, AtomicWriteFile.
- COV subscriptions. SubscribeCOV creates state on the device, so v1 is polling only (see Open Questions).
- Alarm and event handling, schedules/calendars, and BACnet/SC or BACnet/IPv6.
- Serving as a BACnet server or device. The app has no device identity: it never sends I-Am and never answers Who-Is.

## 3. Architecture

```
┌──────────────────────── explorer (single Go binary) ────────────────────────┐
│                                                                             │
│  Web UI (embedded SPA) ◄──► HTTP API (REST + SSE)                           │
│                                   │                                         │
│          ┌────────────────────────┼─────────────────────────┐               │
│          ▼                        ▼                         ▼               │
│    Discovery svc            Browse svc (on-demand)     Poller/Scheduler     │
│          │                        │                         │               │
│          └───────────────► BACnet Client ◄──────────────────┘               │
│                     (invoke-ID mgmt, timeouts, retries,                     │
│                      segmentation, RPM→RP fallback, per-device limits)      │
│                                   │                                         │
│                     Transport (one UDP socket :47808)                       │
│                     BBMD foreign-device registration                        │
│                     Routing table (DNET → router IP)                        │
│                                                                             │
│   Store (pgx) ◄── Discovery / Browse / Poller (batched sample writer)       │
└─────────────────────────────────────────────────────────────────────────────┘
                                    │
                         TimescaleDB / Postgres
```

Package layout (proposed):

| Package | Responsibility |
|---|---|
| `client/` (library) | Transport, request/response correlation, segmentation, BBMD, routing. Reusable outside the app. |
| `cmd/explorer/` | `main`: config, wiring, graceful shutdown |
| `internal/explorer` | The source-neutral core: node model, `Source` interface, registry and events (see explorer-sources-spec.md) |
| `internal/source/bacnet` | The BACnet source: Who-Is sweeps, I-Am handling, add-by-address, device/object enumeration, rendering |
| `internal/poller` | Scheduling, RPM batching, quality/backoff |
| `internal/store` | Postgres schema, migrations, queries, batched sample writer |
| `internal/api` | REST handlers, SSE live-value stream |
| `web/` | SPA source; the build output is embedded with `go:embed` |

## 4. Library prerequisites (work in this repo)

The app needs these library changes first. Today each example does its own socket handling and correlation inline. The detailed design is in [library-upgrade-spec.md](https://github.com/jonalfarlinga/bacnet/blob/main/docs/library-upgrade-spec.md), which supersedes this table where they differ; for example, it sets the Go minimum to 1.25.

| # | Change | Why |
|---|---|---|
| L1 | **`client` package**: one socket owner and a read-loop dispatcher. Replies are matched on (peer, invoke ID). Unconfirmed messages (I-Am) go to subscribers. | Enables concurrent requests. Only one process can own :47808 reliably, especially on Windows. |
| L2 | Configurable APDU timeout and retries (defaults 3 s × 3). Invoke-ID allocator per peer. `context.Context` on every call. | Robust polling |
| L3 | Move `SegmentReassembler` from `examples/` into the library. Send Segment-ACKs automatically. Advertise max-segments/max-APDU in requests. | `object-list` and large RPM responses are segmented |
| L4 | **Routing**: all `New*` builders accept an optional destination `(DNET, DADR)`. Capture `SNET/SADR` from I-Am into the device address. Add Who-Is-Router-To-Network and parse I-Am-Router-To-Network. | MS/TP devices behind routers |
| L5 | **Foreign device**: Register-Foreign-Device with TTL and re-registration at TTL/2. Distribute-Broadcast-To-Network for Who-Is. Handle the BVLC-Result NAK. | Reaching remote subnets |
| L6 | Who-Is with a device-instance range (low/high) | Chunked discovery on large sites |
| L7 | Typed value decoding: Real, Double, Unsigned, Signed, Enumerated, Boolean, CharacterString, BitString (status-flags), Null, Date/Time. Include decoding of RPM per-property errors. | Store and display values generically |
| L8 | A read-only guard: the client refuses to send any service outside an allowlist (Who-Is, RP, RPM, RR, Segment-ACK, BVLC registration/FDT reads) | Enforces the read-only requirement structurally |
| L9 | Raise `go` in `go.mod` to ≥ 1.22 | pgx v5 and modern stdlib (`net/http` routing patterns) |

## 5. Functional requirements

### 5.1 Network & transport
- Bind to a configurable interface/IP and port (default `0.0.0.0:47808`). Derive the broadcast address from the interface.
- Optional BBMD: `address:port` and TTL. Show registration status in the UI.
- Ignore packets from our own interface addresses (the library already does this in its examples).
- Limit concurrency: default 2 in-flight requests per IP device, configurable per device, and 1 per routed network (DNET) shared by every device on it, configurable per network.
- Apply a global rate limit on outbound requests per second (default 50) so the app never floods a trunk.

### 5.2 Discovery
1. **Sweep**: global Who-Is (optionally chunked by instance ranges) plus Who-Is-Router-To-Network. Listen for a configurable window (default 10 s). Periodic re-sweeps (default every 6 h) plus a manual trigger.
2. **Device registry**: upsert on each I-Am with instance, IP:port, SNET/SADR, max-APDU, segmentation support, and vendor ID. Update `last_seen`.
3. **Device enumeration** runs on first discovery and on manual refresh:
   - RPM (or RP fallback) of `object-name`, `vendor-name`, `model-name`, `firmware-revision`, `application-software-version`, `protocol-services-supported`, `max-apdu-length-accepted`, `segmentation-supported`, `location`, `description`.
   - Read `object-list`: try the whole array. If that fails with segmentation-not-supported or abort, read index 0 for the count, then each index.
4. **Object enumeration**: batched RPM of `object-name`, `description`, `units`, `present-value`, `status-flags`, and `out-of-service` for each object. The value type is inferred from `present-value`'s tag.
5. **Disabled sources**: discovery and reads apply to every device found; there is no allow-list. The user can turn off a device, an IP subnet, or a routed network. A disabled source stays in the registry, marked disabled, and the app sends it nothing and ignores what it sends (library deny list).
6. Report progress over SSE (devices found, objects enumerated, errors).

### 5.3 Browsing
- Tree: Network (local / DNET n / BBMD) → Device → Object type group → Object.
- Object detail: read `property-list` if the device supports it, otherwise a known property set for the object type. Show every property with a decoded value. A manual refresh button issues one RPM; there is no background polling unless the point is historized.
- Search across the name, description, device, and `type:instance` fields.

### 5.4 Historizing
- A **trend** means one historized point: (object, property, default `present-value`) plus interval.
- Trend settings: `interval` (min 1 s, default 60 s), `deadband` (absolute, optional; numeric samples within ±deadband of the last stored value are skipped, but at least one sample per `max_gap`, default 15 min), `enabled`.
- Bulk add: select many objects in the tree and apply one interval.
- **Scheduler**:
  - Group due trends by (device, interval bucket).
  - Pack them into RPM requests sized to the device's max-APDU (estimate conservatively). Use RP for devices without RPM.
  - Align sample timestamps to the scheduled tick, not to response arrival. Store the actual read latency separately in stats.
  - Add jitter within 10% of the interval to spread load.
- **Quality**:
  - Each sample carries `quality` (good / stale / comm-fail / property-error) and the raw `status_flags`.
  - On timeout, write no value. Increment the device failure counter. After N consecutive failures (default 3), mark the device offline and back off exponentially up to 10 min. Write one `comm-fail` marker row so charts can show gaps.
- **Writer**: buffer samples in memory and flush every 1 s or every 1,000 rows with `COPY`. If the database is unavailable, buffer up to a bound (default 100k rows) and then drop the oldest, logging a warning.

### 5.5 Charts & export
- Trend viewer: select 1–8 trends and a time range (presets plus custom). Use raw data for ranges under 2 days and continuous aggregates (min/avg/max) otherwise.
- Show binary and multi-state points as step lines.
- CSV export of the selected trends and range (raw or aggregated).

### 5.6 Diagnostics
- Packet log: ring buffer of the last N (default 5,000) packets with timestamp, direction, peer, hex, and decoded summary (via `bacnet.Parse`). Filter by device or service. Optional pcap export.
- Device health: success rate, average/p95 latency, last error, and online/offline state.
- BBMD: registration status and a Read-FDT / Read-BDT viewer.

## 6. Data model (TimescaleDB)

```sql
CREATE TABLE devices (
  id               BIGSERIAL PRIMARY KEY,
  device_instance  INTEGER NOT NULL UNIQUE,     -- 0..4194302
  name             TEXT,
  ip               INET NOT NULL,
  port             INTEGER NOT NULL,
  snet             INTEGER,                      -- NULL = local
  sadr             BYTEA,
  vendor_id        INTEGER,
  vendor_name      TEXT,
  model_name       TEXT,
  firmware         TEXT,
  max_apdu         INTEGER,
  segmentation     SMALLINT,
  services_supported BIT VARYING,
  max_concurrency  SMALLINT,                    -- per-device override
  status           TEXT NOT NULL DEFAULT 'online', -- online|offline|unknown
  first_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE objects (
  id           BIGSERIAL PRIMARY KEY,
  device_id    BIGINT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  object_type  INTEGER NOT NULL,
  instance     INTEGER NOT NULL,
  name         TEXT,
  description  TEXT,
  units        INTEGER,
  value_type   SMALLINT,            -- application tag of present-value
  last_enumerated TIMESTAMPTZ,
  UNIQUE (device_id, object_type, instance)
);

CREATE TABLE trends (
  id           BIGSERIAL PRIMARY KEY,
  object_id    BIGINT NOT NULL REFERENCES objects(id) ON DELETE RESTRICT,
  property_id  INTEGER NOT NULL DEFAULT 85,    -- present-value
  array_index  INTEGER,
  interval_s   INTEGER NOT NULL DEFAULT 60 CHECK (interval_s >= 1),
  deadband     DOUBLE PRECISION,
  max_gap_s    INTEGER NOT NULL DEFAULT 900,
  enabled      BOOLEAN NOT NULL DEFAULT true,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (object_id, property_id, array_index)
);

CREATE TABLE samples (
  time          TIMESTAMPTZ NOT NULL,
  trend_id      BIGINT NOT NULL,
  value_num     DOUBLE PRECISION,   -- real/unsigned/signed/enum/bool (0/1)
  value_text    TEXT,               -- char strings / anything non-numeric
  status_flags  SMALLINT,           -- 4-bit BACnet status-flags
  quality       SMALLINT NOT NULL,  -- 0 good, 1 stale, 2 comm-fail, 3 property-error
  PRIMARY KEY (trend_id, time)
);
SELECT create_hypertable('samples', 'time', chunk_time_interval => INTERVAL '1 day');
ALTER TABLE samples SET (timescaledb.compress, timescaledb.compress_segmentby = 'trend_id');
SELECT add_compression_policy('samples', INTERVAL '7 days');
-- retention configurable, default none
-- continuous aggregates: samples_15m, samples_1h (min/avg/max/last/count per trend)
```

Deleting a trend disables it by default. Hard delete, which also purges samples, is a separate explicit action.

Migrations: embedded SQL files applied at startup (`golang-migrate` or `goose`).

## 7. HTTP API (JSON)

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/status` | Transport, BBMD, DB, poller stats |
| POST | `/api/discovery` | Start a sweep `{lowInstance?, highInstance?, window_s?}` |
| GET | `/api/devices` | List with filters (`status`, `network`, `q`) |
| GET | `/api/devices/{id}` | Device detail and health |
| POST | `/api/devices/{id}/refresh` | Re-enumerate device and objects |
| GET | `/api/devices/{id}/objects` | Objects (paginated, filter by type / q) |
| GET | `/api/objects/{id}/properties` | Live read of all properties |
| GET | `/api/trends` | List trends with last sample and health |
| POST | `/api/trends` | Create one or many `{objectIds[], propertyId?, interval_s, deadband?}` |
| PATCH | `/api/trends/{id}` | Update interval, deadband, enabled |
| DELETE | `/api/trends/{id}?purge=bool` | Disable, or purge |
| GET | `/api/trends/{id}/samples` | `from`, `to`, `agg=raw|15m|1h` |
| GET | `/api/export.csv` | `trendIds`, `from`, `to`, `agg` |
| GET | `/api/diag/packets` | Packet log (filterable) |
| GET | `/api/diag/fdt` | Read-FDT from the configured BBMD |
| GET | `/api/events` | SSE: discovery progress, live values for historized points, device up/down |

## 8. Web UI

Stack recommendation: **React + TypeScript + Vite**, **TanStack Query** for data fetching, and **uPlot** for time-series charts (fast with 100k+ points). The build output goes to `web/dist` and is embedded.

Screens:
1. **Explorer**: left pane is the network tree with search. The right pane shows device or object detail, a property table with a refresh button, and a "Historize" toggle with an interval picker.
2. **Trends**: table of historized points (device, object, interval, last value/time, quality, error rate). Supports bulk enable/disable, interval changes, and delete.
3. **Charts**: trend picker, time range, overlay chart, and CSV export.
4. **Diagnostics**: packet log, device health table, BBMD/FDT.
5. **Settings** (read from the config file; changes that need a restart are flagged).

## 9. Configuration

`explorer.yaml`. Any key can be overridden by an env var (`EXPLORER_…`).

```yaml
bacnet:
  interface: "Ethernet"        # or bind_ip
  port: 47808
  apdu_timeout: 3s
  apdu_retries: 3
  max_rps: 50
  bbmd:
    address: ""                # "10.1.2.3:47808" enables foreign-device mode
    ttl: 300s
discovery:
  resweep_every: 6h
  whois_window: 10s
  chunk_size: 0                # 0 = single global Who-Is
poller:
  default_interval: 60s
  offline_after_failures: 3
  max_backoff: 10m
store:
  dsn: "postgres://explorer:***@localhost:5432/bacnet"
  flush_interval: 1s
  flush_rows: 1000
  buffer_limit: 100000
http:
  listen: ":8080"
  auth: none                   # see Open Questions
```

## 10. Non-functional

- **Scale target**: 500 devices, 50k objects, 10k trends at 60 s, which is about 170 samples/s. The poller is expected to be bounded by device and MS/TP throughput, not by the database.
- **Deployment**: single binary for Linux and Windows (Windows service via `kardianos/service`). A `docker-compose.yml` runs Timescale and the explorer. On Docker, BACnet broadcast needs host networking.
- **Observability**: structured logs (`slog`) and a Prometheus `/metrics` endpoint (requests, timeouts, poll lag, write lag).
- **Testing**:
  - The library layer is tested with pcap fixtures (`traffcaps/`).
  - The client is tested against `bacnet-stack` server apps in the Vagrant setup, including its gateway app for routed devices (library-upgrade-spec §8).
  - The store is tested with testcontainers.

## 11. Milestones

1. **M1 – Client library**: L1–L3, L7, L8. A CLI `explorer probe <ip>` reads device info.
2. **M2 – Discovery + store**: L4–L6, discovery service, schema, and migrations. Devices and objects are persisted.
3. **M3 – API + Explorer UI**: tree, detail, and live property reads.
4. **M4 – Historian**: poller, sample writer, and Trends screen.
5. **M5 – Charts, export, diagnostics, packaging**.

## 12. Open questions

1. **COV**: allow SubscribeCOV as an opt-in per trend later? It changes only the device's subscription list, not any values, but it is technically a write of state.
2. **Auth**: none (trusted LAN), a single shared password, or OIDC?
3. **Multi-site**: one explorer per site writing to a shared database? That would need a `site_id` on `devices`.
4. **Retention**: a default retention period, or keep everything?
5. **Change tracking**: should object renames and removals discovered on re-enumeration be kept as history, or just overwritten?
6. **Non-present-value properties**: which ones do users actually want to historize (e.g. `status-flags`, `priority-array`, `reliability`)?
