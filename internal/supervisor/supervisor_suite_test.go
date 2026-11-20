package supervisor_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestSupervisorScenarios(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Supervised process lifecycle")
}
