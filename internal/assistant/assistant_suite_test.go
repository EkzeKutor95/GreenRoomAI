package assistant_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestAssistantScenarios(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GREEN ROOM bounded AI interpreter")
}
