package local_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLocalProvider(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Local Provider Suite")
}
