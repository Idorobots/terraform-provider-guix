// SPDX-License-Identifier: MPL-2.0

package guix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Source struct {
	Configuration string
	Channels      string
	System        string
	Files         map[string]string
}

func (s Source) Digest() string {
	b, _ := json.Marshal(s)
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

// Backend makes Guix operations independently testable from Terraform and SSH.
type Backend interface {
	BuildImage(context.Context, Source, string, string) (string, string, error)
	BuildSystem(context.Context, Source) (string, error)
	Deploy(context.Context, Source, Destination) error
}

type Destination struct {
	Address, User, IdentityFile, PrivateKey, HostKey string
	Port                                             int
}

type Client struct{ Runner Runner }

var StorePathPattern = regexp.MustCompile(`^/gnu/store/[0-9a-z]{32}-[^/\s]+$`)

func ValidSourcePath(name string) bool {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	// Reserve the provider's top-level filenames and all their descendants.
	first := strings.Split(name, string(filepath.Separator))[0]
	return first != "configuration.scm" && first != "channels.scm" && first != "machines.scm" && first != "identity"
}

func workspace(s Source) (string, error) {
	dir, err := os.MkdirTemp("", "terraform-guix-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	files := map[string]string{"configuration.scm": s.Configuration, "channels.scm": s.Channels}
	for name, contents := range s.Files {
		if !ValidSourcePath(name) {
			return "", fmt.Errorf("invalid or reserved source path %q", name)
		}
		files[name] = contents
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(files[name]), 0600); err != nil {
			return "", err
		}
	}
	ok = true
	return dir, nil
}

func (c Client) invoke(ctx context.Context, dir string, args ...string) (string, error) {
	prefix := []string{"time-machine", "-C", filepath.Join(dir, "channels.scm"), "--", "-L", dir}
	return run(ctx, c.Runner, dir, "guix", append(prefix, args...)...)
}

func storePath(output string) (string, error) {
	p := strings.TrimSpace(output)
	if !StorePathPattern.MatchString(p) {
		return "", fmt.Errorf("guix returned an invalid store path: %q", output)
	}
	return p, nil
}

func (c Client) BuildImage(ctx context.Context, source Source, imageType, root string) (string, string, error) {
	dir, err := workspace(source)
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return "", "", err
	}
	out, err := c.invoke(ctx, dir, "system", "image", "--image-type="+imageType, "--system="+source.System, "--root="+root, filepath.Join(dir, "configuration.scm"))
	if err != nil {
		return "", "", err
	}
	p, err := storePath(out)
	if err != nil {
		return "", "", err
	}
	checksum, err := Checksum(ctx, p)
	return p, checksum, err
}

func Checksum(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		_, _ = h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c Client) BuildSystem(ctx context.Context, source Source) (string, error) {
	dir, err := workspace(source)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	out, err := c.invoke(ctx, dir, "system", "build", "--system="+source.System, filepath.Join(dir, "configuration.scm"))
	if err != nil {
		return "", err
	}
	return storePath(out)
}

// SchemeString emits a Scheme string literal, not executable source.
func SchemeString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 32 || r == 127 {
				fmt.Fprintf(&b, `\x%x;`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (c Client) Deploy(ctx context.Context, source Source, destination Destination) error {
	dir, err := workspace(source)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	identity := destination.IdentityFile
	if destination.PrivateKey != "" {
		identity = filepath.Join(dir, "identity")
		if err := os.WriteFile(identity, []byte(destination.PrivateKey), 0600); err != nil {
			return err
		}
	}
	if identity == "" {
		return fmt.Errorf("an SSH private_key or identity_file is required")
	}
	identity, err = filepath.Abs(identity)
	if err != nil {
		return err
	}
	definition := `(use-modules (gnu machine) (gnu machine ssh))
(list (machine
  (operating-system (load ` + SchemeString(filepath.Join(dir, "configuration.scm")) + `))
  (environment managed-host-environment-type)
  (configuration (machine-ssh-configuration
    (host-name ` + SchemeString(destination.Address) + `)
    (system ` + SchemeString(source.System) + `)
    (user ` + SchemeString(destination.User) + `)
    (port ` + strconv.Itoa(destination.Port) + `)
    (identity ` + SchemeString(identity) + `)
    (host-key ` + SchemeString(destination.HostKey) + `)))))
`
	if err := os.WriteFile(filepath.Join(dir, "machines.scm"), []byte(definition), 0600); err != nil {
		return err
	}
	_, err = c.invoke(ctx, dir, "deploy", filepath.Join(dir, "machines.scm"))
	return err
}
