//go:build !linux

package runner

import (
	"errors"
	"net/http"
)

func vsockClient(_ uint32) (*http.Client, string, error) {
	return nil, "", errors.ErrUnsupported
}
