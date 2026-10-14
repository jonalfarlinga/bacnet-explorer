# BACnet explorer

A web UI for browsing a BACnet network, in the manner of YABE, built on the
[`bacnet`](https://github.com/jonalfarlinga/bacnet) library. On load it broadcasts a Who-Is and lists the
devices that answer. Picking a device reads its object list and hangs the objects under it, with their
names, present values and status flags; devices behind a router hang off the router, under their network
number. Picking an object reads all of its properties.

It is read-only: it sends Who-Is, ReadProperty and ReadPropertyMultiple requests, the Segment-ACKs that
segmented replies need, Who-Is-Router-To-Network to find the router for a network, and with `-bbmd`, the
foreign-device registration and the table reads of the BBMD, and refuses to send anything else. It does not
write, subscribe, send an I-Am or answer what devices send to it. That is the explorer's rule, not the
library's: it opens the client with `ReadOnly`, without which the client also takes COV subscriptions.

## Building and running

The UI is a React app in `web/` that is compiled into the binary, so it has to be built first (a current Node
LTS):

    # Build the UI into web/dist.
    $ cd web
    $ npm install
    $ npm run build
    $ cd ..

    # Browse the network on an adapter, given by name or by IPv4 address.
    $ go run . -interface "Ethernet 3"

    # Or try it on simulated devices, with no network involved.
    $ go run . -sim

Then open http://127.0.0.1:8080. Without `-interface` the UI asks which adapter to use, unless there is only
one, and remembers the answer. The explorer needs UDP port 47808 on that adapter to hear the I-Ams, so it
cannot share an adapter with another BACnet program such as YABE. `-h` lists the other flags.

Until the library's v0.4.0 is tagged, `go.mod` builds against a checkout of the library in the directory
beside this one (`../bacnet`), through a `replace` directive. Once it is tagged, drop the directive and
`go get github.com/jonalfarlinga/bacnet@v0.4.0`.

A device that no Who-Is reaches, because it is on another IP subnet or does not announce itself, can be
listed by its address instead: **Add by address** in the header takes an IP (with `:port` when it is not
47808), and `-add 10.22.60.3,10.22.60.4` does the same at startup. The explorer reads what the device's
I-Am would have said and goes on as for any other device. The device must answer unicast from the machine,
which across subnets needs an IP route to it; a device so listed is marked "added by address".

### The store and the admin panel
The explorer keeps what it finds in a directory, `-store` (`data` unless set). Every device and point it
discovers goes into `temp.json` there, with what the source needs to reach it again, as it is discovered.
The **Admin** button in the header lists the points by the decision taken about them: new, tracked or
sidelined. Pick points, in batch, and press Track or Sideline. Each decision writes the whole registry anew to
`registry_<datetime>.json`, so the latest file is the registry in force and the older ones are its history;
the decided points, with their devices, leave `temp.json`. The decision is for the data consumers: the
explorer shows every point either way, marked `new`, `tracked` or `sidelined`.

A registry file is never written again, and is read-only from the moment it appears. The **History** tab of
the admin panel lists them all, the one in force first, with what each holds and how it differs from the
decisions now. **Restore** puts an older one back in force by writing it as a new registry file, so the
history only grows: a point decided about since is `new` again, and one removed since comes back, marked
"not responding" until its device is read.

On start, the explorer loads the latest registry and `temp.json` first and only then starts discovery.
Everything loaded is marked "not responding" until it is heard from: a device when it answers the Who-Is
or is read again (one listed by address is read again at once), a point when its device's points are read.
A point the admin decided about stays listed if its device no longer has it, marked not responding, so that
the loss is seen rather than silent.

**Remove** in the admin panel forgets the picked points, for ones that have moved or are gone: they leave the
registry and the tree. The button asks twice. A point the device still has comes back as new the next time
the device is read.

### The background walk
Opening a device in the UI reads its points at once. The explorer also reads every device's points on its
own, in the background, so that the registry fills without anyone clicking: the walk takes devices in turn,
never walked first, then longest ago, and reads each one's object list and points. It is paced by a budget of
points a minute, `-walk` (60 unless set; 0 turns it off), counted per object touched, so a device of 240
points takes four minutes and the network barely notices. It stands aside whenever a device is being read
for someone else, and comes back to a device after `-walk-again` (a day). The admin panel's footer shows
where it stands.

### The daily sweep
A device that neither answers the Who-Is nor was added by its address is found by trying addresses. The
sweep is off until it is given ranges: `-sweep 0.1-255.254`, or **Daily sweep** in the settings the UI asks
for. A range is over the last two bytes of the address only, first to last, and several go with commas
(`4.1-4.254,9.1-11.254`); the first two bytes are always the adapter's own, so a sweep cannot be pointed at
another network. Once a day, at `-sweep-at` (02:00 unless set; `now` sweeps ten seconds after the network
is opened and a day after each start), the explorer sends each address one ReadProperty for the Device
object's identifier, through the wildcard instance, on the UDP port it listens on. It sends it once: an
address that does not answer is not tried again until the next day, and nothing is kept about it. A device
that answers and is not listed yet is read and listed as one added by address is.

The sweep is paced at `-sweep-rate` addresses a second (10 unless set), so all 65,000 of `0.1-255.254` take
about two hours. Left out are every address a device is already listed at, whether it answered in this run or is kept in
the store from an earlier one, the machine's own address, the adapter's subnet address and broadcast address,
and outside the adapter's subnet, where the subnets are not known, the addresses ending in .0 and .255. A
device too old to be read through the wildcard instance answers with an error; the explorer then sends that
one address a Who-Is, and lists the device if its I-Am comes back. If it does not, the address is named in
the status. The status is in the admin panel's footer, under the walk's, and in the tooltip of the header's
BACnet/IP badge: the ranges, when the next sweep starts, and for the running one and the last one, when it
started, the address it tried most recently, how many it has tried, and what it found. A running sweep's
line is brought up to date every two seconds.

To the network a sweep looks like a port scan of UDP 47808, and every unused address on the adapter's subnet
costs an ARP broadcast: tell whoever watches the network before giving it ranges, and keep to the ranges
where devices can be. It finds BACnet/IP devices only: when one of them is a router, the devices behind it
are not listed by the sweep.

### Routers, BBMDs and diagnostics
When a device that should answer does not appear, run with `-log debug`: it prints every I-Am heard and every
datagram dropped, with the reason, so you can tell a device that never answered from one whose answer was
refused. `-frames file` writes every datagram sent and received to a file in hex, which the library's test
fixtures can be made from.

Devices behind BACnet routers are found through the Who-Is, which is addressed to every network; the
explorer learns which router serves each network from their replies, and asks with Who-Is-Router-To-Network
when it has to. A BACnet network on another IP subnet, which broadcasts do not reach, is reached through its
BBMD: with `-bbmd 10.0.124.2` the explorer registers with it as a foreign device, sends its broadcasts to it as
Distribute-Broadcast-To-Network, and gets the subnet's broadcasts back forwarded. The registration lasts
`-bbmd-ttl` (5 minutes unless set) and is renewed halfway through; if the BBMD refuses it or stops answering,
the explorer tries again with a growing backoff, from 5 seconds to 5 minutes.

The library's README describes a two-subnet Vagrant scenario with a `bacnet-stack` BBMD on `foovm` and a
device on `barvm`. From the host, the explorer registers with the BBMD and finds `barvm` through it:

    $ go run . -interface 10.0.123.1 -bbmd 10.0.124.2

What to check: the explorer prints that it is a foreign device of the BBMD; device 125 appears, at
10.0.124.3 and not at the BBMD's address; its objects read; and without `-bbmd`, the same Who-Is finds
nothing, which proves the client is foreign.

## Working on it

To work on the UI, run the explorer and then `npm run dev` in `web/`: Vite serves the UI with hot reload and
passes `/api` on to the explorer.

| Directory | What is in it |
|---|---|
| `.` | The program: flags and wiring (`main.go`) |
| `internal/explorer` | The core: the node model, the `Source` interface, and the registry that holds what every source has found and streams it as events. It knows no protocol. |
| `internal/source/bacnet` | The BACnet/IP source: discovery, add-by-address, the daily sweep, how a device's objects and properties are read, and how values are rendered |
| `internal/api` | The HTTP API and the event stream the UI listens to: sources and nodes, nothing protocol-shaped |
| `web` | The UI: a tree of nodes and a detail pane, drawn from what the sources describe |

The simulated devices behind `-sim`, which the tests also use, are the library's `client/sim` package.

This is the browsing part of [docs/explorer-spec.md](docs/explorer-spec.md), built on the source-neutral
core of [docs/explorer-sources-spec.md](docs/explorer-sources-spec.md): a second protocol is a second
`Source`, and the core, API and UI do not change for it. Historizing, charts and the packet log in the
explorer spec are not built yet.

The explorer was split out of the library's repository on 2026-10-14, at its commit 2a371eb; the commits
here are that history, cut down to the explorer's files.
