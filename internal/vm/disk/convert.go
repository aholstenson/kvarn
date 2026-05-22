package disk

import (
	"fmt"
	"os"

	"github.com/lima-vm/go-qcow2reader/convert"
	"github.com/lima-vm/go-qcow2reader/image/qcow2"
)

// ConvertQcow2ToRaw converts a qcow2 disk image to a sparse raw image.
func ConvertQcow2ToRaw(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open qcow2 source: %w", err)
	}
	defer srcFile.Close()

	img, err := qcow2.Open(srcFile, nil)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer img.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create raw destination: %w", err)
	}
	defer func() {
		if err != nil {
			dstFile.Close()
			os.Remove(dst)
		}
	}()

	if err = dstFile.Truncate(img.Size()); err != nil {
		return fmt.Errorf("truncate raw image: %w", err)
	}

	if err = convert.Convert(dstFile, img, convert.Options{}); err != nil {
		return fmt.Errorf("convert qcow2 to raw: %w", err)
	}

	if err = dstFile.Close(); err != nil {
		return fmt.Errorf("close raw image: %w", err)
	}

	return nil
}
