// SPDX-License-Identifier: MPL-2.0

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
)

var ErrUnavailable = errors.New("host unavailable")
var hostnamePattern = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+local$`)

func ValidHostname(s string) bool { return len(s) <= 253 && hostnamePattern.MatchString(s) }

// Resolver is the only discovery contract consumed by host lifecycle code.
// A native mDNS implementation can replace Avahi without changing the schema.
type Resolver interface {
	Resolve(context.Context, string) (string, error)
}

type Avahi struct{ Runner guix.Runner }

func (a Avahi) Resolve(ctx context.Context, hostname string) (string, error) {
	if !ValidHostname(hostname) {
		return "", fmt.Errorf("expected an mDNS hostname ending in .local: %q", hostname)
	}
	out, stderr, err := a.Runner.Run(ctx, "", "avahi-resolve-host-name", "-4", hostname)
	if err != nil {
		var missing *exec.Error
		if errors.As(err, &missing) {
			return "", fmt.Errorf("Avahi is not installed: %w", err)
		}
	}
	// avahi-resolve can report resolution failure on stderr while exiting zero.
	if ctx.Err() != nil || strings.Contains(stderr, "Timeout reached") || strings.Contains(stderr, "DNS failure: NXDOMAIN") {
		return "", fmt.Errorf("%w: %s", ErrUnavailable, hostname)
	}
	if err != nil {
		return "", fmt.Errorf("Avahi discovery failed: %w: %s", err, stderr)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || !strings.EqualFold(fields[0], hostname) || net.ParseIP(fields[1]).To4() == nil {
		return "", fmt.Errorf("invalid Avahi IPv4 response: %q", out)
	}
	return fields[1], nil
}
