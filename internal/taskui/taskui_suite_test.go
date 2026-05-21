package taskui

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTaskUI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TaskUI Suite")
}
