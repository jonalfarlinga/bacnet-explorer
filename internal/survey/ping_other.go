//go:build !windows

package survey

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Elsewhere a ping is the system's ping command, once per address: a raw
// socket would need root, and the unprivileged ICMP socket is not on every
// system. It exits 0 for a reply, and says in its output when a router
// answered instead.
var unreachableFrom = regexp.MustCompile(`[Ff]rom (\d+\.\d+\.\d+\.\d+)`)

func ping(ctx context.Context, to netip.Addr, timeout time.Duration) (echo, error) {
	secs := max(1, int(math.Ceil(timeout.Seconds())))
	wait := strconv.Itoa(secs)
	if runtime.GOOS == "darwin" {
		wait = strconv.Itoa(secs * 1000)
	}
	out, err := exec.CommandContext(ctx, "ping", "-c", "1", "-W", wait, to.String()).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return echo{alive: true, from: to}, nil
	case errors.As(err, &exit):
		var from netip.Addr
		if m := unreachableFrom.FindStringSubmatch(string(out)); m != nil {
			from, _ = netip.ParseAddr(m[1])
		}
		text := strings.ToLower(string(out))
		switch {
		case strings.Contains(text, "host unreachable"):
			return echo{noHost: true, from: from}, nil
		case strings.Contains(text, "net unreachable"), strings.Contains(text, "network unreachable"):
			return echo{noNet: true, from: from}, nil
		}
		return echo{}, nil
	}
	return echo{}, fmt.Errorf("ping: %w", err)
}
