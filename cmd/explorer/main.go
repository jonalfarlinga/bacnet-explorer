// explorer serves a web UI for browsing a BACnet network: the devices that
// answer a Who-Is, and the objects and properties of the ones you open.
//
//	go run ./cmd/explorer -interface "Ethernet 3"
//	go run ./cmd/explorer -sim
//
// It is read-only on the BACnet network. It sends Who-Is, ReadProperty and
// ReadPropertyMultiple requests and the Segment-ACKs that segmented replies
// need, and nothing else: it does not write, subscribe, announce itself with
// an I-Am, or answer what devices send to it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
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
	flagSim       = flag.Bool("sim", false, "browse simulated devices on this machine instead of a network")
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
	svc := discovery.New()
	defer svc.Close()

	use := func(ifc client.Interface) error {
		cfg := ifc.Config(uint16(*flagPort))
		cfg.Timeout, cfg.Retries = *flagTimeout, *flagRetries
		if err := svc.Use(ifc.Name, cfg, false); err != nil {
			if inUse(err) {
				return fmt.Errorf("UDP port %d on %s (%s) is in use: another BACnet program on this machine has it open", cfg.Local.Port(), ifc.Name, cfg.Local.Addr())
			}
			return fmt.Errorf("opening UDP %s on %s: %w", cfg.Local, ifc.Name, err)
		}
		fmt.Printf("BACnet/IP on %s (%s), broadcasting to %s\n", ifc.Name, cfg.Local, cfg.Broadcast)
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
		}, true)
		if err != nil {
			return err
		}
		fmt.Println("browsing simulated devices at", network.Addr())
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
