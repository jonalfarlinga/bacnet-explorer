// explorer serves a web UI for browsing what a building's equipment
// exposes: today the BACnet/IP devices that answer a Who-Is or are given by
// address, and the objects and properties of the ones you open.
//
//	go run ./cmd/explorer -interface "Ethernet 3"
//	go run ./cmd/explorer -sim
//
// A network on another IP subnet is reached through its BBMD, or its devices
// are listed by address:
//
//	go run ./cmd/explorer -interface "Ethernet 3" -bbmd 10.0.124.2
//	go run ./cmd/explorer -interface "Ethernet 3" -add 10.22.60.3,10.22.60.4
//
// Devices that neither answer a Who-Is nor are listed are found by a sweep,
// once a day, of the addresses given by their last two bytes; the first two
// are the adapter's:
//
//	go run ./cmd/explorer -interface "Ethernet 3" -sweep 0.1-255.254
//
// It is read-only on the BACnet network. It sends Who-Is, ReadProperty and
// ReadPropertyMultiple requests, the Segment-ACKs that segmented replies
// need, Who-Is-Router-To-Network to find routers, with -bbmd, the
// registration that makes the BBMD forward broadcasts to it, and with -sweep,
// one ReadProperty to each address swept; nothing else.
// It does not write, subscribe, announce itself with an I-Am, or answer what
// devices send to it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonalfarlinga/bacnet/client"
	"github.com/jonalfarlinga/bacnet/client/sim"
	"github.com/jonalfarlinga/bacnet/internal/api"
	"github.com/jonalfarlinga/bacnet/internal/explorer"
	"github.com/jonalfarlinga/bacnet/internal/source/bacnet"
	"github.com/jonalfarlinga/bacnet/web"
)

var (
	flagHTTP      = flag.String("http", "127.0.0.1:8080", "address the web UI is served on")
	flagInterface = flag.String("interface", "", "network adapter to use, by name or IPv4 address; chosen in the UI if left out and there is more than one")
	flagPort      = flag.Uint("port", client.DefaultPort, "BACnet/IP UDP port")
	flagTimeout   = flag.Duration("timeout", 3*time.Second, "how long to wait for each reply")
	flagRetries   = flag.Int("retries", 2, "how many times to resend a request that got no reply")
	flagBBMD      = flag.String("bbmd", "", "register as a foreign device with the BBMD at this address (IP or IP:port) to reach a BACnet network on another IP subnet")
	flagTTL       = flag.Duration("bbmd-ttl", client.DefaultTTL, "how long each foreign-device registration lasts; it is renewed halfway through")
	flagAdd       = flag.String("add", "", "comma-separated IP addresses (IP or IP:port) of devices to list by reading them, for those that no Who-Is reaches, such as devices on other IP subnets")
	flagSweep     = flag.String("sweep", "", "addresses to try once a day with one read each, for devices that neither answer a Who-Is nor are listed: ranges over the last two bytes, as 0.1-255.254 or 4.1-4.254,9.1-11.254; the first two bytes are the adapter's; no sweep if left out")
	flagSweepAt   = flag.String("sweep-at", "02:00", "time of day the sweep starts, or \"now\": when the explorer opens the network, then a day after each start")
	flagSweepRate = flag.Float64("sweep-rate", bacnet.DefaultSweepPerSec, "addresses the sweep tries in a second")
	flagSim       = flag.Bool("sim", false, "browse simulated devices on this machine instead of a network")
	flagWalk      = flag.Float64("walk", 60, "the background walk's budget in points a minute: it reads every device's points in turn, this slowly, standing aside while anyone else reads; 0 turns it off")
	flagWalkAgain = flag.Duration("walk-again", 24*time.Hour, "how old a device's last walk must be before the walk reads it again")
	flagStore     = flag.String("store", "data", "directory the explorer keeps what it finds in: temp.json for what is undecided, and a registry_<datetime>.json for each decision; empty runs without one")
	flagLog       = flag.String("log", "", "print the client's log to stderr at this level: debug shows every I-Am heard and every datagram dropped, with why; info, routes and the BBMD registration; warn, refused sends and failures")
	flagFrames    = flag.String("frames", "", "file to log every datagram sent and received to, in hex, for a capture without Wireshark")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "explorer:", err)
		os.Exit(1)
	}
}

// inUse reports whether err is a bind to an address that is taken. The error
// is a different one on each operating system, so it goes by the text.
func inUse(err error) bool {
	text := err.Error()
	return strings.Contains(text, "address already in use") || strings.Contains(text, "Only one usage of each socket address")
}

func run() error {
	if *flagPort == 0 || *flagPort > 0xFFFF {
		return fmt.Errorf("-port %d is not a UDP port", *flagPort)
	}
	if *flagSweepRate <= 0 {
		return fmt.Errorf("-sweep-rate %v is not a rate", *flagSweepRate)
	}
	opts := bacnet.Options{Timeout: *flagTimeout, Retries: *flagRetries, SweepPerSec: *flagSweepRate}
	if *flagLog != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(*flagLog)); err != nil {
			return fmt.Errorf("-log: %w", err)
		}
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	if *flagFrames != "" {
		f, err := os.OpenFile(*flagFrames, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("-frames: %w", err)
		}
		defer f.Close()
		var mu sync.Mutex
		opts.OnPacket = func(e client.PacketEvent) {
			dir := "to"
			if e.Inbound {
				dir = "from"
			}
			mu.Lock()
			fmt.Fprintf(f, "%s %s %s %x\n", e.Time.Format("15:04:05.000"), dir, e.Peer, e.Data)
			mu.Unlock()
		}
	}
	var adds []string
	if *flagAdd != "" {
		for _, s := range strings.Split(*flagAdd, ",") {
			if _, err := bacnet.ParseAddress(s, 0, ""); err != nil {
				return fmt.Errorf("-add: %w", err)
			}
			adds = append(adds, strings.TrimSpace(s))
		}
	}
	if *flagSim {
		network, err := sim.Start()
		if err != nil {
			return fmt.Errorf("starting the simulated devices: %w", err)
		}
		defer network.Close()
		opts.Sim = network
	}

	reg := explorer.New()
	defer reg.CloseAll()
	if *flagStore != "" {
		st, err := explorer.OpenStore(*flagStore)
		if err != nil {
			return err
		}
		if err := reg.UseStore(st); err != nil {
			return err
		}
		info := st.Info()
		known := len(reg.Nodes())
		if info.Latest != "" {
			fmt.Printf("store %s: %d nodes from %s and temp.json\n", info.Dir, known, info.Latest)
		} else {
			fmt.Printf("store %s: %d nodes from temp.json\n", info.Dir, known)
		}
	}
	src := bacnet.New(opts)
	reg.Register(src)
	ctx := context.Background()

	// open starts the BACnet source and lists what -add named.
	open := func(settings explorer.Values) error {
		if err := reg.Open(ctx, bacnet.Kind, settings); err != nil {
			if inUse(err) {
				return fmt.Errorf("UDP port %d on %s is in use: another BACnet program on this machine has it open", *flagPort, settings["adapter"])
			}
			return err
		}
		fmt.Println(src.Status().Summary)
		for _, address := range adds {
			go func() {
				ctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				n, err := reg.Add(ctx, bacnet.Kind, explorer.Values{"address": address})
				if err != nil {
					fmt.Fprintln(os.Stderr, "explorer: -add:", err)
					return
				}
				fmt.Printf("added %s at %s\n", n.Label, address)
			}()
		}
		return nil
	}

	settings := explorer.Values{
		"adapter":  *flagInterface,
		"port":     strconv.FormatUint(uint64(*flagPort), 10),
		"bbmd":     *flagBBMD,
		"bbmd_ttl": flagTTL.String(),
		"sweep":    *flagSweep,
		"sweep_at": *flagSweepAt,
	}
	switch {
	case *flagSim:
		if err := open(nil); err != nil {
			return err
		}
	case *flagInterface != "":
		if err := open(settings); err != nil {
			return err
		}
	default:
		// With one adapter there is nothing to choose.
		if list, err := client.Interfaces(); err == nil && len(list) == 1 {
			settings["adapter"] = list[0].Name
			if err := open(settings); err != nil {
				fmt.Fprintln(os.Stderr, "explorer:", err)
			}
		}
	}

	reg.StartWalk(explorer.WalkConfig{PointsPerMinute: *flagWalk, Again: *flagWalkAgain})

	ln, err := net.Listen("tcp", *flagHTTP)
	if err != nil {
		return err
	}
	fmt.Printf("explorer is at http://%s\n", ln.Addr())

	srv := &http.Server{Handler: api.New(api.Config{Registry: reg, UI: web.UI()})}
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	go func() {
		<-interrupted
		srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
