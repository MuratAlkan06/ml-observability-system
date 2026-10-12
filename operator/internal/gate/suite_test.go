package gate

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The engine is pure, so its suite starts no envtest: plain go test runs it,
// with the controller suite's Ginkgo tables and no clock (D44, D45).
func TestGate(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gate Suite")
}
