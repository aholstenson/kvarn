package disk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// qcow2Magic is the file signature every qcow2 image starts with.
var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

// qcow2HeaderSize covers the fixed fields common to v2 and v3, up to and
// including the virtual size at offset 24.
const qcow2HeaderSize = 32

// Qcow2VirtualSize reports the guest-visible size of a qcow2 image, which is
// independent of both how many bytes the file occupies on the host and of any
// backing file it reads through to. The size is a fixed header field, so this
// reads the first 32 bytes rather than opening the image and its backing
// chain.
func Qcow2VirtualSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open qcow2 image: %w", err)
	}
	defer f.Close()

	header := make([]byte, qcow2HeaderSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		return 0, fmt.Errorf("read qcow2 header of %q: %w", path, err)
	}

	if !bytes.Equal(header[0:4], qcow2Magic) {
		return 0, fmt.Errorf("%q is not a qcow2 image", path)
	}
	if version := binary.BigEndian.Uint32(header[4:8]); version != 2 && version != 3 {
		return 0, fmt.Errorf("unsupported qcow2 version %d in %q", version, path)
	}

	size := binary.BigEndian.Uint64(header[24:32])
	if size > math.MaxInt64 {
		return 0, fmt.Errorf("qcow2 virtual size %d in %q exceeds int64", size, path)
	}

	return int64(size), nil
}
