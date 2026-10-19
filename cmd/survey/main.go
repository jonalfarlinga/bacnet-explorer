// survey takes stock of IP addresses before a BACnet device is added at
// one. It tries every address of an adapter's subnet, or of the ranges
// given, as the explorer's daily sweep does, with one ReadProperty each,
// and writes a CSV with a row per address: the BACnet devices with their
// names, the other hosts, the vacant addresses, which are where a new
// device can go, and those that answered nothing. The terminal gets the
// devices and the counts.
//
//	go run ./cmd/survey -interface "Ethernet 3"
//	go run ./cmd/survey -interface "Ethernet 3" -range 10.117.0.1-10.117.255.254 -rate 50 -csv site.csv
//
// On the adapter's own subnet a vacant address is one that no BACnet
// device answered for and that the operating system could not resolve by
// ARP either, which a host answers whether or not it answers a ping.
// Beyond the subnet each address is pinged as well, and a router's answer
// that the host is unreachable makes it vacant; one that answers nothing
// at all is only silent.
//
// To the network it looks like a port scan of UDP 47808, and every vacant
// address costs an ARP broadcast: tell whoever watches the network first.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonalfarlinga/bacnet-explorer/internal/survey"
	"github.com/jonalfarlinga/bacnet/client"
)

var (
	flagInterface = flag.String("interface", "", "network adapter whose subnet is surveyed, by name or IPv4 address; the only one if left out and there is only one")
	flagPort      = flag.Uint("port", client.DefaultPort, "BACnet/IP UDP port the devices listen on")
	flagRange     = flag.String("range", "", "addresses to survey instead of the whole subnet: 192.168.1.20-192.168.1.60, or one address, or several with commas; beyond the adapter's subnet they are pinged as well")
	flagRate      = flag.Float64("rate", survey.DefaultPerSec, "addresses tried in a second")
	flagTimeout   = flag.Duration("timeout", 3*time.Second, "how long to wait for each address to answer")
	flagCSV       = flag.String("csv", "", "file to write the survey to, a row per address; survey_<datetime>.csv in the current directory if left out")
	flagLog       = flag.String("log", "", "print the client's log to stderr at this level: debug, info or warn")
	flagQuiet     = flag.Bool("quiet", false, "print no progress while the survey runs")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "survey:", err)
		os.Exit(1)
	}
}

func run() error {
	if *flagPort == 0 || *flagPort > 0xFFFF {
		return fmt.Errorf("-port %d is not a UDP port", *flagPort)
	}
	if *flagRate <= 0 {
		return fmt.Errorf("-rate %v is not a rate", *flagRate)
	}
	ranges, err := survey.ParseRanges(*flagRange)
	if err != nil {
		return fmt.Errorf("-range: %w", err)
	}
	ifc, err := adapter(*flagInterface)
	if err != nil {
		return err
	}
	path := *flagCSV
	if path == "" {
		path = "survey_" + time.Now().Format("20060102-150405") + ".csv"
	}
	// The file is opened first, so that a survey of two hours does not end
	// in a directory it cannot write to.
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	opts := survey.Options{
		Interface: ifc, Port: uint16(*flagPort), Ranges: ranges,
		PerSec: *flagRate, Timeout: *flagTimeout,
	}
	if *flagLog != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(*flagLog)); err != nil {
			return fmt.Errorf("-log: %w", err)
		}
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	if !*flagQuiet {
		announced := false
		opts.Progress = func(p survey.Progress) {
			if !announced {
				announced = true
				takes := time.Duration(float64(p.Total)/(*flagRate)) * time.Second
				fmt.Fprintf(os.Stderr, "  %s to try, about %s at %v a second\n", plural(p.Total, "address"), takes.Round(time.Minute), *flagRate)
				return
			}
			if p.Identifying {
				fmt.Fprintf(os.Stderr, "  all %d addresses tried; reading the %s found\n", p.Tried, plural(p.Devices, "BACnet device"))
				return
			}
			at := ""
			if p.At.IsValid() {
				at = ", at " + p.At.String()
			}
			fmt.Fprintf(os.Stderr, "  %d of %d addresses tried%s: %s, %s so far\n",
				p.Tried, p.Total, at, plural(p.Devices, "BACnet device"), plural(p.Hosts, "other host"))
		}
	}

	// Ctrl-C ends the survey and writes what it found by then.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(ranges) == 0 {
		fmt.Fprintf(os.Stderr, "surveying the subnet of %s, %s, from %s: one read to each address\n", ifc.Name, ifc.Addr.Masked(), ifc.Addr.Addr())
	} else {
		fmt.Fprintf(os.Stderr, "surveying %s through %s, %s: one read to each address, and a ping to each beyond the adapter's subnet\n",
			*flagRange, ifc.Name, ifc.Addr)
	}
	res, err := survey.Run(ctx, opts)
	if res == nil {
		return err
	}
	if err := writeCSV(f, res); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	report(res, path)
	return nil
}

// adapter picks the adapter named, or the only one, or lists them.
func adapter(name string) (client.Interface, error) {
	if name != "" {
		return client.FindInterface(name)
	}
	list, err := client.Interfaces()
	if err != nil {
		return client.Interface{}, err
	}
	if len(list) == 1 {
		return list[0], nil
	}
	var names []string
	for _, ifc := range list {
		names = append(names, fmt.Sprintf("  %-24s %s", ifc.Name, ifc.Addr))
	}
	return client.Interface{}, fmt.Errorf("-interface names which adapter to survey:\n%s", strings.Join(names, "\n"))
}

// report prints the devices and the counts; the rest is in the file.
func report(res *survey.Result, path string) {
	what := res.Subnet.String()
	if len(res.Ranges) > 0 {
		parts := make([]string, len(res.Ranges))
		for i, r := range res.Ranges {
			parts[i] = r.String()
		}
		what = strings.Join(parts, ", ")
	}
	fmt.Printf("%s surveyed from %s", what, res.From)
	if res.From.Port() != res.Port {
		fmt.Printf(" (port %d was in use, so I-Ams broadcast to it were not heard)", res.Port)
	}
	fmt.Printf(": %s tried in %s", plural(res.Tried, "address"), res.Took.Round(time.Second))
	if res.Beyond > 0 {
		fmt.Printf(", %d of them beyond the adapter's subnet %s", res.Beyond, res.Subnet)
	}
	if res.Stopped {
		fmt.Printf(", stopped with %d to go", res.Total-res.Tried)
	}
	fmt.Println()
	if res.PingErr != nil {
		fmt.Printf("Pings could not be sent (%v): beyond the adapter's subnet only BACnet devices are told from silence.\n", res.PingErr)
	}
	if res.NeighborsErr != nil {
		fmt.Printf("The neighbor table could not be read (%v): on the adapter's subnet only BACnet devices are told from silence.\n", res.NeighborsErr)
	}

	fmt.Printf("\nBACnet devices (%d)\n", len(res.Devices))
	for _, d := range res.Devices {
		fmt.Printf("  %-15s  %-17s  %s\n", d.Addr, d.MAC, describe(d))
	}

	fmt.Printf("\n%s besides this machine, %s", plural(len(res.Hosts), "other host"), plural(len(res.Vacant), "vacant address"))
	if len(res.Unrouted) > 0 {
		fmt.Printf(", %s with no network", plural(len(res.Unrouted), "address"))
	}
	if len(res.Silent) > 0 {
		fmt.Printf(", %s that answered nothing", plural(len(res.Silent), "address"))
	}
	fmt.Printf(": a row for each in %s\n", path)
	if len(res.Vacant) > 0 {
		// What the vacant addresses rest on: the neighbor table for those
		// on the subnet, a router's word for those beyond it.
		onSubnet := 0
		for _, a := range res.Vacant {
			if _, byRouter := res.By[a]; !byRouter {
				onSubnet++
			}
		}
		var basis []string
		if onSubnet > 0 {
			basis = append(basis, fmt.Sprintf("nothing in the neighbor table for %d on the subnet", onSubnet))
		}
		var routers []string
		for r, n := range res.Routers {
			if r.IsValid() {
				routers = append(routers, fmt.Sprintf("%s for %d", r, n))
			} else {
				routers = append(routers, fmt.Sprintf("an unknown router for %d", n))
			}
		}
		if len(routers) > 0 {
			sort.Strings(routers)
			basis = append(basis, "a router answered the ping that nobody is there: "+strings.Join(routers, ", "))
		}
		fmt.Println("Vacant: " + strings.Join(basis, "; ") + ".")
	}
	if len(res.Silent) > 0 && res.Beyond > 0 {
		fmt.Println("Silent: no BACnet device, no ping reply, and no router's word either way; vacant, or a host that answers neither.")
	}
}

// describe says which device is at an address, and what it routes.
func describe(d survey.Device) string {
	which := "would not say which device it is"
	if !d.Unnamed {
		which = fmt.Sprintf("device %d", d.Instance)
		if d.Name != "" {
			which += "  " + d.Name
		}
		if made := strings.TrimSpace(strings.Join([]string{d.Vendor, d.Model}, " ")); made != "" {
			which += "  (" + made + ")"
		}
	}
	if r := routes(d); r != "" {
		which += "; routes " + r
	}
	return which
}

// routes lists the networks a router serves, with the devices heard on each.
func routes(d survey.Device) string {
	if len(d.Routed) == 0 {
		return ""
	}
	nets := make([]int, 0, len(d.Routed))
	for n := range d.Routed {
		nets = append(nets, int(n))
	}
	sort.Ints(nets)
	parts := make([]string, len(nets))
	for i, n := range nets {
		parts[i] = fmt.Sprintf("network %d: %s", n, plural(d.Routed[uint16(n)], "device"))
	}
	return strings.Join(parts, ", ")
}

// writeCSV writes a row per address tried, in address order: what is at
// it, and what that rests on.
func writeCSV(f *os.File, res *survey.Result) error {
	w := csv.NewWriter(f)
	if err := w.Write([]string{"address", "status", "basis", "mac", "instance", "name", "vendor", "model", "routes"}); err != nil {
		return err
	}
	rows := map[netip.Addr][]string{}
	for _, d := range res.Devices {
		instance, basis := strconv.FormatUint(uint64(d.Instance), 10), "answered as a BACnet device"
		if d.Unnamed {
			instance, basis = "", "answered as a BACnet device, but not which"
		}
		rows[d.Addr] = []string{d.Addr.String(), "bacnet", basis, d.MAC, instance, d.Name, d.Vendor, d.Model, routes(d)}
	}
	for _, h := range res.Hosts {
		basis := "in the neighbor table"
		if h.Ping {
			basis = "answered a ping"
		}
		rows[h.Addr] = []string{h.Addr.String(), "host", basis, h.MAC}
	}
	by := func(a netip.Addr, what string) string {
		if r, ok := res.By[a]; ok {
			if r.IsValid() {
				return what + " from " + r.String()
			}
			return what
		}
		return "not in the neighbor table"
	}
	for _, a := range res.Vacant {
		rows[a] = []string{a.String(), "vacant", by(a, "host unreachable")}
	}
	for _, a := range res.Unrouted {
		rows[a] = []string{a.String(), "unrouted", by(a, "network unreachable")}
	}
	for _, a := range res.Silent {
		rows[a] = []string{a.String(), "silent", "answered nothing"}
	}
	rows[res.Local] = []string{res.Local.String(), "host", "this machine"}
	addrs := make([]netip.Addr, 0, len(rows))
	for a := range rows {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].Less(addrs[j]) })
	for _, a := range addrs {
		row := rows[a]
		for len(row) < 9 {
			row = append(row, "")
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "s") {
		noun += "e"
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
