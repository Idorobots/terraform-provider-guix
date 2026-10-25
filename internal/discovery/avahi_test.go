// SPDX-License-Identifier: MPL-2.0

package discovery

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

type runner struct {
	out, stderr string
	err         error
}

func (r runner) Run(context.Context, string, string, ...string) (string, string, error) {
	return r.out, r.stderr, r.err
}

func TestAvahiFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		runner               runner
		unavailable, failure bool
	}{
		{"resolved", runner{out: "node.local\t192.0.2.1\n"}, false, false},
		{"offline", runner{stderr: "Failed to resolve host name 'node.local': Timeout reached", err: errors.New("exit 1")}, true, true},
		{"timeout with exit zero", runner{stderr: "Failed to resolve host name 'node.local': Timeout reached"}, true, true},
		{"nxdomain with exit zero", runner{stderr: "Failed to resolve host name 'node.local': DNS failure: NXDOMAIN"}, true, true},
		{"dbus", runner{stderr: "Failed to create client object: Daemon not running", err: errors.New("exit 1")}, false, true},
		{"missing", runner{err: &exec.Error{Name: "avahi-resolve-host-name", Err: exec.ErrNotFound}}, false, true},
		{"malformed", runner{out: "node.local not-an-ip"}, false, true},
		{"empty without resolution error", runner{}, false, true},
		{"other hostname", runner{out: "other.local 192.0.2.1"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, err := (Avahi{Runner: tc.runner}).Resolve(t.Context(), "node.local")
			if (err != nil) != tc.failure || errors.Is(err, ErrUnavailable) != tc.unavailable {
				t.Fatalf("address=%q err=%v", address, err)
			}
		})
	}
}
