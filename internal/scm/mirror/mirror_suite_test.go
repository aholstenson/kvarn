package mirror_test

import (
	"testing"

	"github.com/aholstenson/kvarn/internal/logging"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMirror(t *testing.T) {
	RegisterFailHandler(Fail)
	logging.SetupForWriter(GinkgoWriter)
	RunSpecs(t, "Mirror Suite")
}
