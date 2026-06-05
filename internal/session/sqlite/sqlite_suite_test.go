package sqlite_test

import (
	"testing"

	"github.com/aholstenson/kvarn/internal/logging"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSQLiteStore(t *testing.T) {
	RegisterFailHandler(Fail)
	logging.SetupForWriter(GinkgoWriter)
	RunSpecs(t, "Session SQLite Store Suite")
}
