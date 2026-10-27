//go:build !linux && !darwin && !freebsd

package push

import (
	"fmt"
	"os"
)

func lockSpool(string) (*os.File, error) {
	return nil, fmt.Errorf("usage push spool is not supported on this platform")
}
