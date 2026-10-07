// explorer serves a web UI for browsing a BACnet network: the devices that
// answer a Who-Is, and the objects and properties of the ones you open.
//
//	go run ./cmd/explorer -interface "Ethernet 3"
//	go run ./cmd/explorer -sim
//
// A network on another IP subnet is reached through its BBMD:
//
//	go run ./cmd/explorer -interface "Ethernet 3" -bbmd 10.0.124.2
//
// It is read-only on the BACnet network. It sends Who-Is, ReadProperty and
// ReadPropertyMultiple requests, the Segment-ACKs that segmented replies
// need, Who-Is-Router-To-Network to find routers, and with -bbmd, the
// registration that makes the BBMD forward broadcasts to it; nothing else.
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
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/api"
	"github.com/jonalfarlinga/bacnet/internal/client"
	"github.com/jonalfarlinga/bacnet/internal/discovery"
	"github.com/jonalfarlinga/bacnet/internal/sim"
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
	flagSim       = flag.Bool("sim", false, "browse simulated devices on this machine instead of a network")
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

// parseAddrPort reads an IP address with an optional port, which is
// defaultPort if left out.
func parseAddrPort(s string, defaultPort uint16) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap, nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%q is not an IP address or IP:port", s)
	}
	return netip.AddrPortFrom(addr, defaultPort), nil
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
	var foreign *client.ForeignDeviceConfig
	if *flagBBMD != "" {
		bbmd, err := parseAddrPort(*flagBBMD, uint16(*flagPort))
		if err != nil {
			return fmt.Errorf("-bbmd: %w", err)
		}
		foreign = &client.ForeignDeviceConfig{BBMD: bbmd, TTL: *flagTTL}
	}
	var logger *slog.Logger
	if *flagLog != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(*flagLog)); err != nil {
			return fmt.Errorf("-log: %w", err)
		}
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	var onPacket func(client.PacketEvent)
	if *flagFrames != "" {
		f, err := os.OpenFile(*flagFrames, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("-frames: %w", err)
		}
		defer f.Close()
		var mu sync.Mutex
		onPacket = func(e client.PacketEvent) {
			dir := "to"
			if e.Inbound {
				dir = "from"
			}
			mu.Lock()
			fmt.Fprintf(f, "%s %s %s %x\n", e.Time.Format("15:04:05.000"), dir, e.Peer, e.Data)
			mu.Unlock()
		}
	}

	var adds []client.Address
	if *flagAdd != "" {
		for _, s := range strings.Split(*flagAdd, ",") {
			addr, err := api.ParseAddress(s, 0, "")
			if err != nil {
				return fmt.Errorf("-add: %w", err)
			}
			adds = append(adds, addr)
		}
	}
	svc := discovery.New()
	defer svc.Close()

	// addAll lists the devices given with -add, once the socket is open.
	addAll := func() {
		for _, addr := range adds {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				dev, err := svc.Add(ctx, addr, 0)
				if err != nil {
					fmt.Fprintln(os.Stderr, "explorer: -add:", err)
					return
				}
				fmt.Printf("added device %d at %s\n", dev.ID, addr)
			}()
		}
	}

	use := func(ifc client.Interface) error {
		cfg := ifc.Config(uint16(*flagPort))
		cfg.Timeout, cfg.Retries = *flagTimeout, *flagRetries
		cfg.ForeignDevice = foreign
		cfg.Logger, cfg.OnPacket = logger, onPacket
		if err := svc.Use(ifc.Name, cfg, false); err != nil {
			if inUse(err) {
				return fmt.Errorf("UDP port %d on %s (%s) is in use: another BACnet program on this machine has it open", cfg.Local.Port(), ifc.Name, cfg.Local.Addr())
			}
			return fmt.Errorf("opening UDP %s on %s: %w", cfg.Local, ifc.Name, err)
		}
		if foreign != nil {
			fmt.Printf("BACnet/IP on %s (%s), a foreign device of the BBMD at %s\n", ifc.Name, cfg.Local, foreign.BBMD)
		} else {
			fmt.Printf("BACnet/IP on %s (%s), broadcasting to %s\n", ifc.Name, cfg.Local, cfg.Broadcast)
		}
		addAll()
		return nil
	}
	cfg := api.Config{Service: svc, UI: web.UI()}

	switch {
	case *flagSim:
		network, err := sim.Start()
		if err != nil {
			return fmt.Errorf("starting the simulated devices: %w", err)
		}
		defer network.Close()
		err = svc.Use("Simulated devices", client.Config{
			Local:     netip.MustParseAddrPort("127.0.0.1:0"),
			Broadcast: network.Addr(),
			Timeout:   *flagTimeout,
			Retries:   *flagRetries,
			Logger:    logger,
			OnPacket:  onPacket,
		}, true)
		if err != nil {
			return err
		}
		fmt.Println("browsing simulated devices at", network.Addr())
		addAll()
	case *flagInterface != "":
		ifc, err := client.FindInterface(*flagInterface)
		if err != nil {
			return err
		}
		if err := use(ifc); err != nil {
			return err
		}
	}
	if !*flagSim {
		cfg.Select = func(adapter string) error {
			ifc, err := client.FindInterface(adapter)
			if err != nil {
				return err
			}
			return use(ifc)
		}
		// With one adapter there is nothing to choose.
		if list, err := client.Interfaces(); err == nil && len(list) == 1 && *flagInterface == "" {
			if err := use(list[0]); err != nil {
				fmt.Fprintln(os.Stderr, "explorer:", err)
			}
		}
	}

	ln, err := net.Listen("tcp", *flagHTTP)
	if err != nil {
		return err
	}
	fmt.Printf("explorer is at http://%s\n", ln.Addr())

	srv := &http.Server{Handler: api.New(cfg)}
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
