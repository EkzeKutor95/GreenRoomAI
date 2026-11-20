package bot_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestBot(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Authorized business bot") }
