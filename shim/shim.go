// Package shim holds the standard `runsnip` shim a stack's infra branch ships beside bin/runsnip-<os>-<arch>: it picks
// the binary for the machine it runs on, so the server needs only sh and uname (never Go).
package shim

import _ "embed"

// Script is the shim, byte for byte what a stack ships as infra/runsnip.
//
//go:embed runsnip
var Script []byte

// Binary is the file the shim runs for an OS and a `uname -m` machine name ("" when none is shipped for it).
func Binary(goos, machine string) string {
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[machine]
	if arch == "" {
		return ""
	}
	return "bin/runsnip-" + goos + "-" + arch
}

// Targets are the binaries a release ships (the server's platforms).
var Targets = []struct{ OS, Arch string }{{"linux", "amd64"}, {"linux", "arm64"}}
