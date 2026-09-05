package nixbase32_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNixBase32(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Nix Base32 Suite")
}
