package cost_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCost(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cost Suite")
}
