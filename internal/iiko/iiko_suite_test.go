package iiko_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestIiko(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Read-only iikoCloud client") }
