// SPDX-License-Identifier: MPL-2.0
package guix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	providerchannel "github.com/SpartanWorks/terraform-provider-guix/channel"
)

type SecretFile struct {
	Content string
	Mode    string
}
type SecretFiles map[string]SecretFile

// Revision is private provisioning state, never a public Guix source digest.
func (files SecretFiles) Revision() string {
	if len(files) == 0 {
		return ""
	}
	body, _ := json.Marshal(struct {
		Files          SecretFiles
		Implementation string
	}{files, providerchannel.Digest()})
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func (c Client) provision(ctx context.Context, channels string, files SecretFiles, destination *Destination, args ...string) error {
	public, err := os.MkdirTemp("", "terraform-guix-extension-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(public)
	private, err := os.MkdirTemp("", "terraform-guix-private-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(private)
	extension := filepath.Join(public, "channel")
	if err := providerchannel.Extract(extension); err != nil {
		return err
	}
	channelFile := filepath.Join(public, "channels.scm")
	if err := os.WriteFile(channelFile, []byte(channels), 0600); err != nil {
		return err
	}
	type entry struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
		Mode        string `json:"mode"`
	}
	manifest := struct {
		Version int     `json:"version"`
		Files   []entry `json:"files"`
	}{Version: 1, Files: []entry{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		file := files[name]
		payload := filepath.Join(private, fmt.Sprintf("payload-%d", i))
		if err := os.WriteFile(payload, []byte(file.Content), 0600); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, entry{payload, name, file.Mode})
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(private, "manifest.json")
	if err := os.WriteFile(manifestPath, body, 0600); err != nil {
		return err
	}
	if destination != nil {
		identity := destination.IdentityFile
		if destination.PrivateKey != "" {
			identity = filepath.Join(private, "identity")
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
		args = append(args, "--host="+destination.Address, "--user="+destination.User,
			"--port="+strconv.Itoa(destination.Port), "--identity="+identity, "--host-key="+destination.HostKey)
	}
	// env is per invocation: never mutate the provider's process environment.
	command := []string{"GUIX_EXTENSIONS_PATH=" + extension, "GUILE_AUTO_COMPILE=0", "guix", "time-machine", "-C", channelFile, "--", "secrets"}
	command = append(command, args...)
	command = append(command, "--manifest="+manifestPath)
	_, err = run(ctx, c.Runner, public, "env", command...)
	return err
}

func (c Client) ProvisionImage(ctx context.Context, channels string, files SecretFiles, input, output string) (string, error) {
	if err := c.provision(ctx, channels, files, nil, "image", "--output="+output, input); err != nil {
		return "", err
	}
	return Checksum(ctx, output)
}

func (c Client) ProvisionHost(ctx context.Context, channels string, files SecretFiles, destination Destination) error {
	if len(files) == 0 {
		return nil
	}
	return c.provision(ctx, channels, files, &destination, "deploy")
}
