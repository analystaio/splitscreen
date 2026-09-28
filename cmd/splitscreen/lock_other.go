//go:build !unix

package main

import "os"

// Advisory locking and ownership are unix concepts; elsewhere edits are
// unserialized, which is acceptable for a development machine.
func lockConfig(string) (func(), error)         { return func() {}, nil }
func preserveOwner(*os.File, os.FileInfo) error { return nil }
