// SPDX-License-Identifier: MPL-2.0

package remote

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/SpartanWorks/terraform-provider-guix/internal/discovery"
	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"golang.org/x/crypto/ssh"
)

func identity(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "test")
	if err != nil {
		t.Fatal(err)
	}
	return signer, string(pem.EncodeToMemory(block))
}

func TestSSHIdentityAndInspection(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		wrongHost, wrongManagement bool
		output                     string
		wantError                  bool
	}{
		{"success", false, false, "/gnu/store/00000000000000000000000000000000-system\n", false},
		{"host mismatch", true, false, "", true},
		{"authentication failure", false, true, "", true},
		{"malformed system", false, false, "not a Guix store path", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, _ := identity(t)
			management, private := identity(t)
			other, otherPrivate := identity(t)
			config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
				if !bytes.Equal(key.Marshal(), management.PublicKey().Marshal()) {
					return nil, fmt.Errorf("unauthorized management key")
				}
				return nil, nil
			}}
			config.AddHostKey(host)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					if channel.ChannelType() != "session" {
						_ = channel.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					stream, requests, err := channel.Accept()
					if err != nil {
						return
					}
					for request := range requests {
						if request.Type == "exec" {
							_ = request.Reply(true, nil)
							_, _ = io.WriteString(stream, tc.output)
							_, _ = stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							_ = stream.Close()
							break
						}
						_ = request.Reply(false, nil)
					}
				}
			}()
			_, portString, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portString)
			if err != nil {
				t.Fatal(err)
			}
			pinned := host.PublicKey()
			if tc.wrongHost {
				pinned = other.PublicKey()
			}
			if tc.wrongManagement {
				private = otherPrivate
			}
			d := guix.Destination{Address: "127.0.0.1", Port: port, User: "root", HostKey: string(ssh.MarshalAuthorizedKey(pinned)), PrivateKey: private}
			system, err := (SSH{}).Inspect(t.Context(), d)
			if (err != nil) != tc.wantError || errors.Is(err, discovery.ErrUnavailable) {
				t.Fatalf("system=%q err=%v", system, err)
			}
			if err == nil && system != strings.TrimSpace(tc.output) {
				t.Fatalf("unexpected system %s", system)
			}
		})
	}
}

// Example invocation:
//
//	GUIX_SSH_ADDRESS=host-or-ip GUIX_SSH_HOST_KEY='ssh-ed25519 ...' \
//	GUIX_SSH_IDENTITY_FILE=/path/to/key go test -v ./internal/remote -run Integration
func TestIntegrationSSH(t *testing.T) {
	address := os.Getenv("GUIX_SSH_ADDRESS")
	if address == "" {
		t.Skip("set GUIX_SSH_ADDRESS for real-host inspection")
	}
	d := guix.Destination{Address: address, Port: 22, User: "root", HostKey: os.Getenv("GUIX_SSH_HOST_KEY"), IdentityFile: os.Getenv("GUIX_SSH_IDENTITY_FILE")}
	system, err := (SSH{}).Inspect(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("observed %s", system)
}
