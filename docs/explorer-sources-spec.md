# Explorer Sources — Deconstruction and Specification (v1 draft)

Companion to [explorer-spec.md](explorer-spec.md) and [library-upgrade-spec.md](library-upgrade-spec.md). This spec takes the explorer apart into the BACnet communication library, a BACnet *source*, and a source-neutral explorer core, so that sources that are not BACnet can be added without the core knowing what they are.

**Status (2026-10-07):** steps 1 to 3 of the delivery plan in §5 are done. The client is public, `internal/explorer` holds the model and registry, `internal/source/bacnet` is the BACnet source, and the API and UI are source-neutral. Two things the implementation settled beyond this text: a node carries a `Listed` flag, set by the registry on nodes that came in a children list, so that a list that replaces a node's children leaves what the source announced on its own (a network behind a router) in place; and node IDs travel in the API as the `id` query parameter, since they hold slashes. Steps 4 and 5 remain.

## 1. Why now

The explorer's job is to show what a building's equipment exposes and, per the explorer spec, to historize points from it. BACnet is the first protocol, not the only one: meters, VFDs and packaged units on the same sites speak Modbus, chillers and switchgear speak SNMP or vendor HTTP APIs, and some data arrives as files. The historian's data model in explorer-spec §6 is keyed on BACnet device instances and object types, so every table and every point reference would have to change later if a second protocol were bolted on after historizing is built. Separating the layers first, while the only consumer is the browsing UI, is the cheap moment to do it.

## 2. What the code is today

The repository is three things that grew together.

### 2.1 The BACnet wire library

Packages `bacnet` (root), `plumbing`, `services` and `objects`: encoding and decoding of BVLC, NPDU, APDU, services and primitive values. No sockets, no state. Importable by anyone. This layer is clean and stays as it is; the library-upgrade spec governs it.

### 2.2 The BACnet client

`client`: one UDP socket, request and reply matching, retries, segment reassembly, the routing table, foreign-device registration, typed value decoding, `Probe`, and the read-only guard. It is BACnet-specific and belongs with the library, as the public `client` package the library-upgrade spec describes in §7. It is internal only because that promotion has not happened. Nothing in it knows about the explorer.

`client/sim` is the BACnet simulator the tests and `-sim` use. It belongs with the client.

### 2.3 The explorer

`internal/discovery`, `internal/api`, `web` and `cmd/explorer`. This is where the two concerns are mixed, and the table below is the inventory of BACnet that has leaked into what should be the neutral part.

| Where | What is BACnet-specific | Lines |
|---|---|---|
| `discovery.Device` | Identity is the device instance (`uint32`). Fields `network`, `mac`, `maxApdu`, `segmentation`, `vendorId`. | model |
| `discovery.Object` | Identity is (object type, instance). Fields `typeName`, `abbr`, `flags` (status-flags). | model |
| `discovery.Property` | Numeric property ID, array index. | model |
| `discovery.Transport` | Adapter, broadcast address, BBMD. | model |
| `Service.Use`, `Scan`, `Add` | Takes a `client.Config`; sends Who-Is; `Probe` by `client.Address`. | ~60 |
| `Service.identify`, `enumerate`, `objectReader` | Reads the Device object's name properties, the object-list, then per-type property sets in RPM batches sized by max-APDU, with RP fallback and the 3-tier error handling. This is the *strategy* for reading a BACnet device and is the bulk of the file. | ~350 |
| `discovery/format.go` | Object type names and abbreviations, property names, units, enum names, status flags, date and time rendering. | 406 |
| `api` | `client.Interfaces` for the adapter list; `client.ObjectID` in routes; `ParseAddress` for IP/network/MAC. | ~60 |
| `web/src` | Tree: devices, routers, networks, object-type groups; chips by type family parsed from `typeName`; detail facts for address, network, MAC, max APDU, segmentation; wording ("Who-Is", "adapter"). | ~300 of 1160 |
| `cmd/explorer` | Every flag: `-interface`, `-port`, `-bbmd`, `-bbmd-ttl`, `-add`, `-log`, `-frames`. | all |

What is already neutral: the event stream and its subscription model, the HTTP plumbing (SSE, JSON, CSRF guard), the tree layout engine in `treeModel.ts` (it lays out rows by depth and kind and does not care what a row is), the filter, and the per-device object-reading state machine (idle, loading, ready, error).

## 3. The target shape

```
┌──────────────────────── explorer (cmd/explorer) ──────────────────────────┐
│  web UI  ◄──►  internal/api  ◄──►  internal/explorer (core)               │
│                                        │ registry of sources, tree,       │
│                                        │ events, (later) historian        │
│                       ┌────────────────┼────────────────┐                 │
│                       ▼                ▼                ▼                 │
│            internal/source/bacnet   source/modbus    source/...           │
│                       │                                                   │
└───────────────────────┼───────────────────────────────────────────────────┘
                        ▼
                 client (library)  ──  plumbing, services, objects
```

Three rules:

1. **The core imports no protocol package.** Not `client`, not `objects`. It knows `Source`, `Node` and `Event`.
2. **A source owns everything protocol-shaped**: transport configuration, discovery, how to read, how to render a value, and the meaning of its node IDs.
3. **Node IDs are opaque strings, scoped by source, and stable.** They are what the historian keys samples on, what the UI selects, and what a URL carries. The core never parses them.

### 3.1 The model (`internal/explorer`)

```go
// A Node is anything shown in the tree: a device, a container, a point.
type Node struct {
    ID       NodeID   // "<source>:<rest>", opaque beyond the prefix
    Parent   NodeID   // "" for a root of the source
    Kind     Kind     // Device, Group, Point
    Label    string   // what the tree shows
    Sublabel string   // the small text beside it: an address, a count
    Family   Family   // Analog, Binary, MultiState, Text, Other; drives the chip
    Facts    []Fact   // key/value pairs for the detail pane, in display order
    Reading  *Reading // for a Point, or a Device that has a headline value
    Children ChildrenState // unknown, loading, ready, error (+ counts, error)
    Error    string   // why the node itself could not be read
}

type Fact struct{ Label, Value string; Mono bool }

type Reading struct {
    Text    string    // rendered by the source: "72.4 °F", "active", "Stage 2"
    Number  *float64  // present when numeric; what the historian stores
    Units   string
    Flags   []string  // quality words the source chose: "in-alarm", "fault", "overridden", "out-of-service"
    Quality Quality   // Good, Stale, CommFail, Error
    At      time.Time
}

// Detail is the full property or register listing behind a node.
type Detail struct{ Rows []DetailRow; ReadAt time.Time }
type DetailRow struct{ Name, Value string; Items []string; Error string }
```

The BACnet `Object.flags` become `Reading.Flags`; `units` and `value` become the `Reading`; `maxApdu`, `segmentation`, `vendorId`, network and MAC become `Facts`. `typeName` and `abbr` become `Label`/`Sublabel` and `Family`. Nothing is lost; it stops being a field name the core has to know.

### 3.2 The source interface

```go
type Source interface {
    // Kind is the ID prefix and the UI's name for the protocol: "bacnet", "modbus".
    Kind() string
    // Describe says how the source is configured and how a node is added by
    // hand, as a form the UI can draw without knowing the protocol.
    Describe() Description
    // Open starts the source with the settings the user gave.
    Open(ctx context.Context, settings Settings) error
    Close() error

    // Discover looks for devices on its own (Who-Is). Optional: a source
    // with nothing to discover returns ErrNotSupported and the UI hides Scan.
    Discover(ctx context.Context) error
    // Add lists a device from a filled-in form (an address, a register map).
    Add(ctx context.Context, form Values) (Node, error)

    // Children reads the nodes under a node: the objects of a device, the
    // registers of a unit. Called when the user opens the node.
    Children(ctx context.Context, id NodeID) ([]Node, error)
    // Detail reads everything behind one node, for the detail pane.
    Detail(ctx context.Context, id NodeID) (Detail, error)
    // Read reads the current readings of several points in one go; the
    // historian's call. A source batches as its protocol allows.
    Read(ctx context.Context, ids []NodeID) ([]Reading, error)

    // Events delivers nodes the source learned about or changed on its own
    // (an I-Am, a refreshed value), and status changes.
    Events() <-chan Event
    Status() Status // transport description, health, source-specific lines
}

type Description struct {
    Settings []Field // what Open needs: adapter, port, BBMD...
    AddForm  []Field // what Add needs: IP, unit ID, register map file...
    CanDiscover bool
}
type Field struct{ Key, Label, Hint string; Type FieldType; Options []string; Required bool }
```

The core keeps a registry of open sources, merges their trees into one, owns the event fan-out to subscribers, and later owns the historian, which calls `Read` on a schedule and stores `Reading.Number` or `Reading.Text` against the `NodeID`.

### 3.3 The BACnet source (`internal/source/bacnet`)

Everything in today's `internal/discovery` and the BACnet parts of `api` and `cmd/explorer` move here, nearly unchanged in logic:

- `Describe`: settings are adapter, port, BBMD, TTL; the add form is IP[:port], optional network and MAC, optional instance. `CanDiscover` is true.
- `Open` builds the `client.Config` from the settings, as `Interface.Config` does now. `Discover` is `WhoIs`. `Add` is `Probe`.
- Node IDs: a device is `bacnet:dev/910200`; its objects `bacnet:dev/910200/ai/3`; a property detail row is not a node. Routers and networks are `Group` nodes the source emits (`bacnet:dev/2000/net/2001`), so the tree shape the UI draws today comes from the source, not from the UI knowing about routing.
- `Children` of a device is today's `enumerate` and `objectReader`; of a network group, the devices behind it.
- `Detail` is today's `Properties` plus `renderProperties`.
- `Read` is an RPM batch of present-value and status-flags, sized by max-APDU, with RP fallback: the loop the explorer spec's scheduler describes in §5.4.
- `format.go` stays here whole. The source renders text; the core shows it.

### 3.4 The API

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/sources` | Each open source: kind, status, description (settings and add form) |
| POST | `/api/sources` | Open a source: `{kind, settings}` |
| DELETE | `/api/sources/{kind}` | Close it |
| POST | `/api/sources/{kind}/discover` | Scan, where supported |
| POST | `/api/sources/{kind}/nodes` | Add by form: `{values}` |
| GET | `/api/nodes` | Every root node |
| GET | `/api/nodes/{id}/children` | Open a node (reads on first call, cached after) |
| POST | `/api/nodes/{id}/refresh` | Read the children again |
| GET | `/api/nodes/{id}/detail` | The detail pane |
| GET | `/api/events` | SSE: `reset`, `node`, `children`, `source` |

Node IDs appear in paths URL-escaped. The routes are the current ones with `devices/{id}/objects/{type}/{instance}` collapsed into `nodes/{id}`.

### 3.5 The UI

- The tree draws `Node`s: label, sublabel, family chip, reading, flags. `treeModel.ts` keeps its layout engine; the `device`/`router`/`network`/`group` row kinds become `Node.Kind` plus whether a node has children.
- The detail pane draws `Facts`, then `Reading`, then `Detail` rows. The BACnet-specific facts arrive as data.
- The header shows one entry per open source with its status line, a Scan button where `CanDiscover`, and an Add button that draws the source's `AddForm`. The adapter prompt becomes the BACnet source's settings form, drawn from `Description.Settings`.
- Wording moves to the sources: "Broadcast a Who-Is" is the BACnet source's hint on its Scan button.

## 4. What this costs and what it protects

The BACnet reading logic, the formatter and the client do not change; they move. The core and API are smaller than what exists, because the node model replaces three JSON shapes with one. The UI changes most, and `treeModel.ts` least.

Checked against explorer-spec §6: the `devices` and `objects` tables become one `nodes` table keyed by `NodeID` with a `source` column and a JSON `facts` column; `trends` references a `NodeID`; `samples` keeps `value_num`, `value_text`, `quality`, and `status_flags` becomes the source's flag words. The schema is simpler, not larger, and a Modbus point fits it on day one.

## 5. Delivery plan

| Step | Content | Ships as |
|---|---|---|
| 1 | Promote `client` and `client/sim` to `client` and `client/sim` (library-upgrade spec §7). Move `Value` decoding and the name lookups into `objects` and `services` as that spec's §5.3 and §5.4 say. | A library release; the explorer imports the public package. |
| 2 | `internal/explorer`: the model, `Source`, the registry and event fan-out. `internal/source/bacnet` implements `Source` over today's discovery code. The old `discovery` package is deleted. An adapter in `api` keeps the current JSON so the UI does not change yet. | Same UI, new core; the tests move with the code. |
| 3 | The API of §3.4 and the UI of §3.5. The adapter from step 2 goes. | The explorer looks the same and is source-neutral. |
| 4 | A second source, to prove the boundary before the historian: Modbus TCP, with a register map given as a file in the add form. It has no discovery, no object list, and no units of its own, which exercises every "optional" in the interface. | `-source modbus` plus the form. |
| 5 | The historian, on `Source.Read` and `NodeID`, per explorer-spec §5.4 with the schema change of §4 above. | The second half of the explorer spec. |

Steps 1 and 2 are independent of each other. Step 4 is chosen over SNMP or a vendor API because it is the protocol most likely on the same sites, has a small Go surface (a few hundred lines over `net`), and is the hardest fit for a model born from BACnet: a Modbus device has no self-description, so everything the tree shows about it comes from configuration. If the model carries that cleanly, it carries anything.

## 6. Persistence: the store and the registry of decisions

Built on 2026-10-07, in the core, since a decision about a point is the same whatever its protocol.

- **Two files in one directory** (`-store`, default `data`). `temp.json` holds every device and point found and not decided about, rewritten a second after the last change. `registry_<datetime>.json` holds the points the admin has decided about, with their devices; every decision writes a new one with everything decided so far, so the latest is the whole registry and the older ones are its history. Both are whole-file JSON, loaded at start, in keeping with the file-based registry the project chose over a database.
- **What a node carries for this**: `Track` (new, tracked, sidelined; points only), `Reached` (heard from or read in this run; false for anything loaded from the store), and `Native`, what the source needs to reach the node again, opaque to the core. The BACnet source puts a device's address, network, MAC, limits and the names it read in `Native`; objects need nothing beyond their ID.
- **`Source.Restore`**: after a source opens, the registry hands it what the store holds of its nodes. The BACnet source makes its device records from `Native`, places their networks, and reads again at once any device that was listed by address, since no Who-Is will reach it. Nothing is marked reached by this; a source says so with events once it hears from a node.
- **Start-up order**: load the latest registry, then `temp.json`, then open the sources. Everything shows at once, unreached, and comes alive as it answers.
- **Lists and decisions**: a children list that no longer holds a decided point does not drop it; the point stays, unreached. An undecided one goes, as before.
- **The admin panel** lists points by decision, grouped by device, and decides in batch over `POST /api/classify`. `GET /api/store` describes the store.
- **Registry files do not change**: one is written beside its name with its permissions already read-only, and moved into place. `temp.json` stays writable.
- **Not persisted**: group nodes, which the source makes again from the devices; readings; children lists beyond the nodes themselves. Whether to keep a device's object list without reading it again was the open question; the answer is yes, as the points themselves, since they are what is decided about and what must show on start.

### 6.1 Removing, and the background walk

- **Remove** (`POST /api/remove`) forgets points that have moved or gone: out of memory, the registry file (written anew) and the temp file. A point the device still has comes back as new on the next read.
- **The walk** (`internal/explorer/walk.go`) reads every reached device's children in the background, one device at a time, never-walked first and then longest ago, and again after `WalkConfig.Again` (a day). It is paced by a point budget, `PointsPerMinute` (60 by default), a token bucket the source waits on for each node it is about to read: `Source.Children` takes a `ChildrenOptions.Limit` for this, nil for a user's read. The BACnet source waits once per object in a batch and once per element of an object list read one at a time. The walk stands aside while any device is being read for someone else, and gives a device half an hour before moving on. `Walked` is kept on the device node, in the store, so a restart does not start the walk over. `GET /api/walk` reports where it stands.

### 6.2 The history of the registry

- **Listing and reading**: `GET /api/registries` describes every registry file, the latest first: name, when written, whether it is in force, and its counts of devices, tracked and sidelined points. A file is counted once, since it never changes. `GET /api/registry?name=` gives one file with its nodes; the name must be that of a registry file in the store, and nothing else is read.
- **Restore** (`POST /api/registry/restore`, `{name}`) puts a file of the history back in force. Only the decisions are taken from it: a point it holds gets the file's decision, a point decided about since and not in it is new again and goes back to `temp.json`, and a point removed since comes back as the file has it, unreached, and is handed to its source through `Source.Restore` if the source is open. What is known of a node now (label, native data, whether it is reached) stays. No file is changed: the result is written as a new registry file, so the registry it replaced is in the history too, and restoring the file in force does nothing.
- **The History tab** of the admin panel lists the files, shows the points of any one by device with the decision it holds beside the decision now, says what restoring it would change, and restores on a second press.

## 7. Decisions to make

1. **Node ID stability across a device's address change.** For BACnet the instance is the identity and the address is a fact, which is right. For Modbus the address is the identity unless the user names the unit in the register map. The register map should carry a `name` that becomes the ID, with the address as a fact, so that moving a meter to another IP keeps its history.
2. **Where configuration lives.** Today everything is flags. With sources that need register maps and named units, a config file (one section per source) replaces most flags; the flags stay for the BACnet adapter and `-sim`. Settle the format at step 3.
3. **Whether the core caches children.** Today the discovery service keeps every device's objects in memory and republishes revisions. Keeping that in the core, keyed by `NodeID`, lets the historian and the UI share one cache, and keeps sources simple: a source reads, the core remembers.
