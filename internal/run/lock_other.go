//go:build !unix

package run

import (
	"errors"
	"os"
)

func flock(*os.File) error   { return errors.New("run locking is not supported on this platform") }
func funlock(*os.File) error { return nil }
