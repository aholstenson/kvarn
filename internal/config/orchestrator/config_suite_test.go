package orchestrator_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestOrchestratorConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Orchestrator Config Suite")
}
