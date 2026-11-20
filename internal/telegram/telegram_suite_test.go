package telegram_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestTelegram(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Telegram delivery") }
