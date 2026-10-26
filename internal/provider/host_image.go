// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

type HostImageModel struct {
	ID               types.String `tfsdk:"id"`
	ArtifactPath     types.String `tfsdk:"artifact_path"`
	Configuration    types.String `tfsdk:"configuration"`
	Channels         types.String `tfsdk:"channels"`
	System           types.String `tfsdk:"system"`
	SystemDefinition types.String `tfsdk:"system_definition"`
	SourceFiles      types.Map    `tfsdk:"source_files"`
}

func hostImageSchema() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Required:            true,
		MarkdownDescription: "Guix image object, normally guix_image.<name>. Defines the host's configuration, channels, target system, supporting files, image identity and manual commissioning file path. Image changes reconcile the host in place.",
		Attributes: map[string]schema.Attribute{
			"id":                schema.StringAttribute{Required: true, MarkdownDescription: "Image resource identity."},
			"artifact_path":     schema.StringAttribute{Required: true, MarkdownDescription: "Image file for manual flashing and booting."},
			"configuration":     schema.StringAttribute{Required: true, MarkdownDescription: "Host operating-system Scheme source."},
			"channels":          schema.StringAttribute{Required: true, MarkdownDescription: "Pinned channels Scheme source."},
			"system":            schema.StringAttribute{Required: true, MarkdownDescription: "Host target architecture."},
			"system_definition": definitionSchema("Scheme source exporting custom target platforms, normally inherited from guix_image. Loaded for system builds and deployment; separate from configuration.", false),
			"source_files":      schema.MapAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Supporting source files."},
		},
	}
}

func readHostImage(ctx context.Context, value types.Object) (HostImageModel, diag.Diagnostics) {
	var image HostImageModel
	var diagnostics diag.Diagnostics
	if value.IsUnknown() {
		image = HostImageModel{
			ID: types.StringUnknown(), ArtifactPath: types.StringUnknown(),
			Configuration: types.StringUnknown(), Channels: types.StringUnknown(),
			System: types.StringUnknown(), SourceFiles: types.MapUnknown(types.StringType),
			SystemDefinition: types.StringUnknown(),
		}
	} else if value.IsNull() {
		diagnostics.AddError("Missing Guix image", "Set image = guix_image.<name> to define the host's desired system and commissioning image.")
	} else {
		diagnostics.Append(value.As(ctx, &image, basetypes.ObjectAsOptions{})...)
	}
	return image, diagnostics
}

func commissioningImageInstruction(imagePath types.String) string {
	if imagePath.IsUnknown() {
		return "Manually write the bootstrap image to the host's boot disk, then boot it. The image file path is known after the image is built; use image.artifact_path."
	}
	if !imagePath.IsNull() {
		return fmt.Sprintf("Manually write the Guix system image file %q to the host's boot disk, then boot it.", imagePath.ValueString())
	}
	return "Manually install and boot the Guix system image supplied by image.artifact_path."
}
