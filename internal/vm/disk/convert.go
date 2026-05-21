package disk

import (
	"os"

	"github.com/cockroachdb/errors"
	"github.com/lima-vm/go-qcow2reader/convert"
	"github.com/lima-vm/go-qcow2reader/image/qcow2"
)

// ConvertQcow2ToRaw converts a qcow2 disk image to a sparse raw image.
func ConvertQcow2ToRaw(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return errors.Wrap(err, "open qcow2 source")
	}
	defer srcFile.Close()

	img, err := qcow2.Open(srcFile, nil)
	if err != nil {
		return errors.Wrap(err, "open qcow2 image")
	}
	defer img.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return errors.Wrap(err, "create raw destination")
	}
	defer func() {
		if err != nil {
			dstFile.Close()
			os.Remove(dst)
		}
	}()

	if err = dstFile.Truncate(img.Size()); err != nil {
		return errors.Wrap(err, "truncate raw image")
	}

	if err = convert.Convert(dstFile, img, convert.Options{}); err != nil {
		return errors.Wrap(err, "convert qcow2 to raw")
	}

	if err = dstFile.Close(); err != nil {
		return errors.Wrap(err, "close raw image")
	}

	return nil
}
