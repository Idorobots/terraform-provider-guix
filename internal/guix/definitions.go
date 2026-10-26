// SPDX-License-Identifier: MPL-2.0

package guix

import _ "embed"

const (
	definitionPrefix       = ".terraform-guix"
	definitionLauncherFile = definitionPrefix + "-launcher.scm"
	imageDefinitionFile    = definitionPrefix + "-image.scm"
	systemDefinitionFile   = definitionPrefix + "-system.scm"
)

//go:embed definitions.scm
var definitionLauncher string
