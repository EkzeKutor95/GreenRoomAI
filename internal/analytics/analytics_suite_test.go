package analytics_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestAnalyticsScenarios(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GREEN ROOM analytics scenarios")
}
