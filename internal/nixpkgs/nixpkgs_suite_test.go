package nixpkgs_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNixpkgs(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Nixpkgs Suite")
}
