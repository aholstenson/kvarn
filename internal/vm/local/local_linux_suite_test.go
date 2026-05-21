//go:build linux

package local_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLocalLinux(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Local Linux Provider Suite")
}
