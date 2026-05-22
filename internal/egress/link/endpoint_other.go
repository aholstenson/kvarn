//go:build !darwin && !linux

package link

import (
	"errors"
	"os"
)

// On unsupported platforms, the OS-specific helpers stub out so the
// package still builds; the local VM provider is also unavailable on
// these platforms.

type unsupportedFrameRW struct{}

func (unsupportedFrameRW) ReadFrame() ([]byte, error) { return nil, errors.ErrUnsupported }
func (unsupportedFrameRW) WriteFrame([]byte) error    { return errors.ErrUnsupported }
func (unsupportedFrameRW) Close() error               { return nil }

func CreateSocketPair() (*os.File, *os.File, error) { return nil, nil, errors.ErrUnsupported }
