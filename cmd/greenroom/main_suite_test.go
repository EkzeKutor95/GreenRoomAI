package main

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestApplicationLifecycle(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Application lifecycle behavior")
}
