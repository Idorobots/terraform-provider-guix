// SPDX-License-Identifier: MPL-2.0

package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SpartanWorks/terraform-provider-guix/internal/discovery"
	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"golang.org/x/crypto/ssh"
)

type Inspector interface {
	Inspect(context.Context, guix.Destination) (string, error)
}
type SSH struct{}

func (SSH) Inspect(ctx context.Context, d guix.Destination) (string, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(d.HostKey))
	if err != nil || len(bytes.TrimSpace(rest)) != 0 {
		return "", fmt.Errorf("host_key must be one OpenSSH public key")
	}
	private := []byte(d.PrivateKey)
	if d.PrivateKey == "" {
		private, err = os.ReadFile(d.IdentityFile)
		if err != nil {
			return "", fmt.Errorf("read SSH identity: %w", err)
		}
	}
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		return "", fmt.Errorf("parse SSH private key: %w", err)
	}
	config := &ssh.ClientConfig{User: d.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(key)}
	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", fmt.Errorf("%w: %v", discovery.ErrUnavailable, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) || errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%w: %v", discovery.ErrUnavailable, err)
		}
		return "", fmt.Errorf("SSH authentication or host verification failed: %w", err)
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	out, err := session.Output("readlink -f /run/current-system")
	if err != nil {
		return "", fmt.Errorf("inspect Guix system: %w", err)
	}
	system := strings.TrimSpace(string(out))
	if !guix.StorePathPattern.MatchString(system) || !strings.HasSuffix(system, "-system") {
		return "", fmt.Errorf("invalid remote Guix system: %q", system)
	}
	return system, nil
}
