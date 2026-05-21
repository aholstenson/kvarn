package limits_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLimits(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Limits Suite")
}
