//go:build !linux && !darwin

package main

import "os"

// isTerminal is not implemented on this platform; reset-admin then
// requires --yes.
func isTerminal(*os.File) bool { return false }
