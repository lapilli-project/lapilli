//go:build !unix

package local

import "io/fs"

func ownedByCurrentUser(fs.FileInfo) bool { return true }
