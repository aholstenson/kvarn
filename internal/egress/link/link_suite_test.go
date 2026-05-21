package link_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLink(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Link Suite")
}
