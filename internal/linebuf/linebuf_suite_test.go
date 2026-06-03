package linebuf_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLinebuf(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Linebuf Suite")
}
