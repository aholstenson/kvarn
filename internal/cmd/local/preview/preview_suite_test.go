package preview

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLocalPreview(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Local Preview Suite")
}
