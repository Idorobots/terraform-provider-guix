// SPDX-License-Identifier: MPL-2.0
package guix

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestProvisioningWorkspaceIsolation(t *testing.T) {
	for _, action := range []string{"image", "deploy"} {
		t.Run(action, func(t *testing.T) {
			const secret = "PRIVATE-PAYLOAD-MARKER"
			const key = "PRIVATE-MANAGEMENT-IDENTITY"
			var public, private string
			stopped := errors.New("stop")
			c := Client{Runner: runnerFunc(func(_ context.Context, dir, binary string, args ...string) (string, string, error) {
				public = dir
				if binary != "env" || !slices.Contains(args, "time-machine") || !slices.Contains(args, "secrets") || !slices.Contains(args, action) {
					t.Fatalf("not a pinned extension invocation: %s %v", binary, args)
				}
				for _, arg := range args {
					if strings.Contains(arg, secret) || strings.Contains(arg, key) {
						t.Fatal("plaintext in argv")
					}
					if strings.HasPrefix(arg, "--manifest=") {
						path := strings.TrimPrefix(arg, "--manifest=")
						private = filepath.Dir(path)
						var m struct {
							Version int
							Files   []struct{ Source, Destination, Mode string }
						}
						body, err := os.ReadFile(path)
						if err != nil || json.Unmarshal(body, &m) != nil || m.Version != 1 || len(m.Files) != 1 {
							t.Fatalf("invalid manifest: %v", err)
						}
						entry := m.Files[0]
						body, err = os.ReadFile(entry.Source)
						if err != nil || string(body) != secret || entry.Destination != "/etc/key" || entry.Mode != "0640" {
							t.Fatal("payload lost")
						}
					}
				}
				if public == private || private == "" || strings.HasPrefix(private, public+"/") {
					t.Fatal("private payloads share public source workspace")
				}
				for _, directory := range []string{public, private} {
					info, err := os.Stat(directory)
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatal("workspace is not private")
					}
				}
				if err := filepath.WalkDir(public, func(path string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !d.IsDir() {
						body, err := os.ReadFile(path)
						if err != nil {
							return err
						}
						if strings.Contains(string(body), secret) || strings.Contains(string(body), key) {
							t.Fatal("payload in public channel/source files")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return "", "", stopped
			})}
			files := SecretFiles{"/etc/key": {Content: secret, Mode: "0640"}}
			var err error
			if action == "image" {
				_, err = c.ProvisionImage(t.Context(), "pinned", files, "/base", "/output")
			} else {
				err = c.ProvisionHost(t.Context(), "pinned", files, Destination{Address: "192.0.2.1", User: "root", Port: 22, HostKey: "public", PrivateKey: key})
			}
			if !errors.Is(err, stopped) {
				t.Fatalf("error lost: %v", err)
			}
			for _, directory := range []string{public, private} {
				if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("temporary workspace retained after failure")
				}
			}
		})
	}
}

func TestSecretRevision(t *testing.T) {
	a := SecretFiles{"/a": {Content: "one", Mode: "0600"}, "/b": {Content: "two", Mode: "0640"}}
	b := SecretFiles{"/b": a["/b"], "/a": a["/a"]}
	if a.Revision() != b.Revision() {
		t.Fatal("revision depends on map order")
	}
	b["/a"] = SecretFile{Content: "one", Mode: "0640"}
	if a.Revision() == b.Revision() {
		t.Fatal("mode change ignored")
	}
	b["/a"] = SecretFile{Content: "changed", Mode: "0600"}
	if a.Revision() == b.Revision() || (SecretFiles{}).Revision() != "" {
		t.Fatal("invalid content or empty revision")
	}
}

func TestProvisioningCancellationAllowsCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	out, _, err := (ExecRunner{}).Run(ctx, "", "env", "sh", "-c",
		`trap 'printf cleanup-completed; exit 0' TERM; while :; do sleep 0.05; done`, "secrets")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(out, "cleanup-completed") {
		t.Fatalf("provisioner was killed before cleanup: output=%q err=%v", out, err)
	}
}

func secretTestIdentity(t *testing.T) (ssh.Signer, string) {
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

// Real Guile-SSH transport and the real public installer, against an isolated
// SSH server. Only the target Guile path and filesystem root are substituted.
func TestChannelSSHProvisioning(t *testing.T) {
	if os.Getenv("GUIX_EVALUATION") != "1" {
		t.Skip("set GUIX_EVALUATION=1 for Guile-SSH provisioning")
	}
	guile, err := exec.LookPath("guile")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"root", "admin", "wrong-host"} {
		t.Run(user, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			host, _ := secretTestIdentity(t)
			management, private := secretTestIdentity(t)
			config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
				if string(key.Marshal()) != string(management.PublicKey().Marshal()) {
					return nil, fmt.Errorf("unauthorized")
				}
				return nil, nil
			}}
			config.AddHostKey(host)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			root := t.TempDir()
			commands := make(chan string, 1)
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
					stream, requests, err := channel.Accept()
					if err != nil {
						return
					}
					for request := range requests {
						if request.Type != "exec" {
							_ = request.Reply(false, nil)
							continue
						}
						var message struct{ Command string }
						_ = ssh.Unmarshal(request.Payload, &message)
						commands <- message.Command
						_ = request.Reply(true, nil)
						command := strings.TrimPrefix(message.Command, "'sudo' '-n' '--' ")
						command = strings.Replace(command, "'/run/current-system/profile/bin/guile'", "'"+guile+"'", 1)
						command = strings.Replace(command, `(install-payload "/" (read))`, fmt.Sprintf(`(install-payload %q (read) #:owner %d #:group %d)`, root, os.Getuid(), os.Getgid()), 1)
						process := exec.CommandContext(ctx, "sh", "-c", command)
						process.Stdin, process.Stdout, process.Stderr = stream, stream, stream.Stderr()
						status := uint32(0)
						if process.Run() != nil {
							status = 1
						}
						_, _ = stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
						_ = stream.Close()
						break
					}
				}
			}()
			pin := host.PublicKey()
			if user == "wrong-host" {
				other, _ := secretTestIdentity(t)
				pin = other.PublicKey()
			}
			address, ok := listener.Addr().(*net.TCPAddr)
			if !ok {
				t.Fatal("expected a TCP listener")
			}
			destination := Destination{Address: "127.0.0.1", User: user, Port: address.Port, PrivateKey: private, HostKey: string(ssh.MarshalAuthorizedKey(pin))}
			// Strip time-machine only in this integration harness: pinning and
			// private-workspace contracts are checked by the runner test above.
			c := Client{Runner: runnerFunc(func(ctx context.Context, dir, binary string, args ...string) (string, string, error) {
				i := slices.Index(args, "secrets")
				return (ExecRunner{}).Run(ctx, dir, binary, append(slices.Clone(args[:2]), append([]string{"guix"}, args[i:]...)...)...)
			})}
			const payload = "private-key\x00\xff\n"
			err = c.ProvisionHost(ctx, "unused", SecretFiles{
				"/etc/private/key": {Content: payload, Mode: "0640"},
				"/etc/private/鍵":   {Content: "", Mode: "0600"},
			}, destination)
			if user == "wrong-host" {
				if err == nil {
					t.Fatal("accepted wrong host key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(root, "etc/private/key"))
			if err != nil || string(body) != payload {
				t.Fatalf("payload not installed: %v", err)
			}
			info, _ := os.Stat(filepath.Join(root, "etc/private/key"))
			if info.Mode().Perm() != 0640 {
				t.Fatal("mode lost over SSH")
			}
			if body, err := os.ReadFile(filepath.Join(root, "etc/private/鍵")); err != nil || len(body) != 0 {
				t.Fatalf("Unicode destination or empty payload lost: %v", err)
			}
			command := <-commands
			if strings.Contains(command, "private-key") || (user == "admin" && !strings.HasPrefix(command, "'sudo' '-n' '--' ")) {
				t.Fatal("payload in command or missing privilege escalation")
			}
		})
	}
}
