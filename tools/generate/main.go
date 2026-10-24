// SPDX-License-Identifier: MPL-2.0

// Command generate renders provider documentation using OpenTofu for CLI work.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(ctx context.Context, dir string, env []string, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func generate() error {
	cli := os.Getenv("TOFU")
	if cli == "" {
		cli = "tofu"
	}
	tofu, err := exec.LookPath(cli)
	if err != nil {
		return fmt.Errorf("OpenTofu is required for documentation generation (TOFU=%q): %w", cli, err)
	}
	tofu, err = filepath.Abs(tofu)
	if err != nil {
		return err
	}
	root, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := command(ctx, root, nil, os.Stdout, tofu, "fmt", "-recursive", "examples/"); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "guix-docs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		return err
	}
	if err := command(ctx, root, nil, os.Stdout, "go", "build", "-o", filepath.Join(bin, "terraform-provider-guix"), "."); err != nil {
		return err
	}
	const address = "registry.opentofu.org/spartanworks/guix"
	configuration := fmt.Sprintf("terraform {\n required_providers {\n guix = { source = %q }\n }\n}\n", address)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(configuration), 0600); err != nil {
		return err
	}
	cliConfig := filepath.Join(dir, ".tofurc")
	override := fmt.Sprintf("provider_installation {\n dev_overrides {\n %q = %q\n }\n direct {}\n}\n", address, bin)
	if err := os.WriteFile(cliConfig, []byte(override), 0600); err != nil {
		return err
	}
	schemaPath := filepath.Join(dir, "schema.json")
	var schema bytes.Buffer
	// Schema queries honor dev_overrides directly; no registry installation or
	// tofu init is needed for this temporary, single-provider configuration.
	if err := command(ctx, dir, []string{"TF_CLI_CONFIG_FILE=" + cliConfig, "TF_IN_AUTOMATION=1"}, &schema, tofu, "providers", "schema", "-json"); err != nil {
		return err
	}
	// File-based tfplugindocs input looks up the local provider name rather
	// than its registry address. Rename this key only in the temporary input.
	var document struct {
		FormatVersion   string                     `json:"format_version"`
		ProviderSchemas map[string]json.RawMessage `json:"provider_schemas"`
	}
	if err := json.Unmarshal(schema.Bytes(), &document); err != nil {
		return fmt.Errorf("decode OpenTofu provider schema: %w", err)
	}
	providerSchema, ok := document.ProviderSchemas[address]
	if !ok {
		return fmt.Errorf("OpenTofu schema is missing %s", address)
	}
	document.ProviderSchemas = map[string]json.RawMessage{"guix": providerSchema}
	contents, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if err := os.WriteFile(schemaPath, contents, 0600); err != nil {
		return err
	}
	tools := filepath.Join(root, "tools")
	if err := command(ctx, tools, nil, os.Stdout, "go", "run", "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs", "generate", "--provider-dir", root, "--provider-name", "guix", "--providers-schema", schemaPath); err != nil {
		return err
	}
	// tfplugindocs supplies Terraform-specific import prose even when the
	// schema came from OpenTofu. Adapt only that generated sentence.
	return filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		updated := strings.ReplaceAll(string(contents), "The [`terraform import` command](https://developer.hashicorp.com/terraform/cli/commands/import)", "The [`tofu import` command](https://opentofu.org/docs/cli/commands/import/)")
		if updated == string(contents) {
			return nil
		}
		return os.WriteFile(path, []byte(updated), 0644)
	})
}
