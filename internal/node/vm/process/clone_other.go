//go:build !linux

package process

import (
	"errors"
	"os"
)

func cloneFile(dst, src *os.File) error { return errors.ErrUnsupported }
