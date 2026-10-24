// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

//go:build generate

package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)

// Format examples and export the provider schema with OpenTofu, then render docs.
// Passing the schema explicitly prevents tfplugindocs from downloading Terraform.
//go:generate go run ./generate
