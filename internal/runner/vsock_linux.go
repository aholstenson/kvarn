//go:build linux

package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/mdlayher/vsock"
)

func vsockClient(port uint32) (*http.Client, string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return vsock.Dial(2, port, nil) // CID 2 = host
			},
		},
	}
	return client, fmt.Sprintf("http://vsock:%d", port), nil
}
