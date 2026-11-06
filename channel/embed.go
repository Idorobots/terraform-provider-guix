// SPDX-License-Identifier: MPL-2.0

// Package channel ships the same Guix extension used by standalone channel users.
package channel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed modules
var embedded embed.FS

var Sources, _ = fs.Sub(embedded, "modules")

func Digest() string {
	h := sha256.New()
	_ = fs.WalkDir(Sources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			body, err := fs.ReadFile(Sources, name)
			if err != nil {
				return err
			}
			_, _ = h.Write([]byte(name + "\x00"))
			_, _ = h.Write(body)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

func Extract(directory string) error {
	return fs.WalkDir(Sources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		path := filepath.Join(directory, name)
		if entry.IsDir() {
			return os.MkdirAll(path, 0700)
		}
		body, err := fs.ReadFile(Sources, name)
		if err != nil {
			return err
		}
		return os.WriteFile(path, body, 0600)
	})
}
