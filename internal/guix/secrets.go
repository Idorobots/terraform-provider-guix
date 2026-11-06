// SPDX-License-Identifier: MPL-2.0
package guix

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// SecretConfiguration is private provisioning input, never a Guix build input.
// FileDigests are explicit Terraform change triggers; Scheme is evaluated only
// by the installed extension during apply.
type SecretConfiguration struct {
	Expression  string
	FileDigests map[string]string
}

func (c Client) CheckSecrets(ctx context.Context, source Source) error {
	dir, err := workspace(source)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := c.invoke(ctx, dir, "secrets", "--help"); err != nil {
		return fmt.Errorf("guix secrets extension is unavailable: include the terraform-provider-guix channel in blueprint.channels and install it with guix pull; the extension must be available inside the pinned guix time-machine environment: %w", err)
	}
	return nil
}

func (c Client) provision(ctx context.Context, source Source, secrets SecretConfiguration, destination *Destination, args ...string) error {
	dir, err := workspace(source)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	private, err := os.MkdirTemp("", "terraform-guix-private-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(private)
	declaration := filepath.Join(private, "secrets.scm")
	if err := os.WriteFile(declaration, []byte(secrets.Expression), 0600); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	args = append(args, "--secrets="+declaration, "--secrets-directory="+cwd)
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
		definition, err := deploymentDefinition(source, dir, *destination, identity)
		if err != nil {
			return err
		}
		machines := filepath.Join(private, "machines.scm")
		if err := os.WriteFile(machines, []byte(definition), 0600); err != nil {
			return err
		}
		args = append(args, machines)
	} else {
		args = append(args, filepath.Join(dir, "configuration.scm"))
	}
	_, err = c.invoke(ctx, dir, "secrets", args...)
	return err
}

func (c Client) ProvisionImage(ctx context.Context, source Source, secrets SecretConfiguration, imageType, target, input, output string) (string, error) {
	args := []string{"image", "--artifact=" + input, "--output=" + output}
	if target != "" {
		args = append(args, "--target="+target)
	} else {
		args = append(args, "--system="+source.System)
	}
	if imageType != "" {
		args = append(args, "--image-type="+imageType)
	}
	if err := c.provision(ctx, source, secrets, nil, args...); err != nil {
		return "", err
	}
	return Checksum(ctx, output)
}

func (c Client) ProvisionHost(ctx context.Context, source Source, secrets SecretConfiguration, destination Destination) error {
	if secrets.Expression == "" {
		return nil
	}
	return c.provision(ctx, source, secrets, &destination, "deploy", "--system="+source.System)
}
