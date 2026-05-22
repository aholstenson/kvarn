//go:build embedrunner

package runnerbin_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRunnerbin(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Runnerbin Suite")
}
