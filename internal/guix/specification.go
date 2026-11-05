// SPDX-License-Identifier: MPL-2.0

package guix

import (
	"fmt"
	"path/filepath"
)

// Evaluate the selected source separately from the generated driver, retaining
// normal Guix loading semantics and source locations for relative local-file.
func specificationDefinition(source Source, dir string) (string, error) {
	kind := source.Kind
	if kind == "" {
		kind = "os"
	}
	predicate, record := "", ""
	switch kind {
	case "os":
		predicate, record = "operating-system?", "operating-system"
	case "image":
		predicate, record = "image?", "image"
	case "machine":
		predicate, record = "machine?", "machine"
	default:
		return "", fmt.Errorf("unknown blueprint specification kind %q", kind)
	}
	definition := `(use-modules (gnu system) (gnu image)
             (gnu machine) (gnu machine ssh)
             (guix scripts system))
(define specification
  (read-operating-system ` + SchemeString(filepath.Join(dir, "specification.scm")) + `))
(unless (` + predicate + ` specification)
  (error "blueprint.` + kind + ` must evaluate to a Guix ` + record + ` record"))
`
	if kind == "machine" {
		definition += `(unless (and (eq? (machine-environment specification)
                         managed-host-environment-type)
             (machine-ssh-configuration? (machine-configuration specification)))
  (error "blueprint.machine must use the managed SSH host environment"))
(unless (operating-system? (machine-operating-system specification))
  (error "blueprint.machine must contain an operating-system"))
`
	}
	return definition, nil
}

func configurationDefinition(source Source, dir string) (string, error) {
	definition, err := specificationDefinition(source, dir)
	if err != nil {
		return "", err
	}
	if source.Kind == "machine" {
		return definition + "(machine-operating-system specification)\n", nil
	}
	return definition + "specification\n", nil
}
