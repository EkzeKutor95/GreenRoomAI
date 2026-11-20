package supervisor_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/supervisor"
)

// TestSupervisorChildProcess is selected only by synthetic supervised children.
// It cannot start the actual application or access Telegram/iiko configuration.
func TestSupervisorChildProcess(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 || len(os.Args) < index+4 || os.Args[index+1] != "supervisor-test-child" {
		return
	}
	mode, path := os.Args[index+2], os.Args[index+3]
	appendEvent := func(event string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(70)
		}
		if _, err = f.WriteString(event + "\n"); err != nil {
			os.Exit(71)
		}
		if err = f.Close(); err != nil {
			os.Exit(72)
		}
	}
	if mode == "fail" {
		appendEvent("start")
		os.Exit(23)
	}
	if mode == "exit" {
		appendEvent("start")
		os.Exit(0)
	}
	active := path + ".active"
	f, err := os.OpenFile(active, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		appendEvent("overlap")
		os.Exit(73)
	}
	if err = f.Close(); err != nil {
		os.Exit(74)
	}
	appendEvent("start")
	if _, err = io.Copy(io.Discard, os.Stdin); err != nil {
		os.Exit(75)
	}
	appendEvent("eof")
	if mode == "hang" {
		for {
			time.Sleep(time.Hour)
		}
	}
	// Keep the old process alive briefly after receiving shutdown: starting its
	// replacement on pipe closure instead of Wait would produce an overlap.
	time.Sleep(30 * time.Millisecond)
	appendEvent("stop")
	if err = os.Remove(active); err != nil {
		os.Exit(76)
	}
	os.Exit(0)
}

func events(path string) []string {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	return strings.Fields(string(data))
}

var _ = Describe("One supervised process", func() {
	var executable, path string
	var logger *slog.Logger
	var cfg supervisor.Config
	BeforeEach(func() {
		var err error
		executable, err = os.Executable()
		Expect(err).NotTo(HaveOccurred())
		path = filepath.Join(GinkgoT().TempDir(), "events")
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		cfg = supervisor.Config{Executable: executable, Args: []string{"-test.run=^TestSupervisorChildProcess$", "--", "supervisor-test-child", "graceful", path}, RestartInterval: 80 * time.Millisecond, ShutdownTimeout: 2 * time.Second}
		// Race-instrumented helper processes should not add the runtime's one-second
		// exit sleep to a deliberately short lifecycle test.
		GinkgoT().Setenv("GORACE", "atexit_sleep_ms=0")
	})
	When("the restart interval expires", func() {
		It("waits for graceful child completion before starting its replacement", func() {
			runner, err := supervisor.New(cfg, logger, supervisor.WithOutput(io.Discard, io.Discard))
			Expect(err).NotTo(HaveOccurred())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan struct{})
			var result error
			go func() { result = runner.Run(ctx); close(finished) }()
			DeferCleanup(func() { cancel(); Eventually(finished, 5*time.Second).Should(BeClosed()) })
			Eventually(func() int {
				count := 0
				for _, event := range events(path) {
					if event == "start" {
						count++
					}
				}
				return count
			}, 5*time.Second).Should(BeNumerically(">=", 2))
			cancel()
			Eventually(finished, 5*time.Second).Should(BeClosed())
			Expect(result).To(Succeed())
			record := events(path)
			Expect(record).NotTo(ContainElement("overlap"))
			active := false
			for _, event := range record {
				switch event {
				case "start":
					Expect(active).To(BeFalse())
					active = true
				case "stop":
					Expect(active).To(BeTrue())
					active = false
				}
			}
			Expect(active).To(BeFalse())
		})
	})
	When("the owner cancels the supervisor", func() {
		It("closes the child's control pipe, waits and never starts another poller", func() {
			cfg.RestartInterval = time.Hour
			runner, err := supervisor.New(cfg, logger, supervisor.WithOutput(io.Discard, io.Discard))
			Expect(err).NotTo(HaveOccurred())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan struct{})
			var result error
			go func() { result = runner.Run(ctx); close(finished) }()
			DeferCleanup(func() { cancel(); Eventually(finished, 5*time.Second).Should(BeClosed()) })
			Eventually(func() []string { return events(path) }, 5*time.Second).Should(Equal([]string{"start"}))
			cancel()
			Eventually(finished, 5*time.Second).Should(BeClosed())
			Expect(result).To(Succeed())
			Expect(events(path)).To(Equal([]string{"start", "eof", "stop"}))
		})
	})
	When("cancellation happens before startup", func() {
		It("does not spawn a child", func() {
			runner, err := supervisor.New(cfg, logger, supervisor.WithOutput(io.Discard, io.Discard))
			Expect(err).NotTo(HaveOccurred())
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			Expect(runner.Run(ctx)).To(Succeed())
			Expect(events(path)).To(BeEmpty())
		})
	})
	When("the child exits with a failure before its restart time", func() {
		It("returns its exit error and does not retry the configuration or application failure", func() {
			cfg.Args[len(cfg.Args)-2] = "fail"
			cfg.RestartInterval = time.Hour
			runner, err := supervisor.New(cfg, logger, supervisor.WithOutput(io.Discard, io.Discard))
			Expect(err).NotTo(HaveOccurred())
			err = runner.Run(context.Background())
			var exit *exec.ExitError
			Expect(errors.As(err, &exit)).To(BeTrue())
			Expect(exit.ExitCode()).To(Equal(23))
			Expect(events(path)).To(Equal([]string{"start"}))
		})
	})
	When("the child completes normally before its restart time", func() {
		It("ends supervision without creating a restart loop", func() {
			cfg.Args[len(cfg.Args)-2] = "exit"
			cfg.RestartInterval = time.Hour
			runner, err := supervisor.New(cfg, logger, supervisor.WithOutput(io.Discard, io.Discard))
			Expect(err).NotTo(HaveOccurred())
			Expect(runner.Run(context.Background())).To(Succeed())
			Expect(events(path)).To(Equal([]string{"start"}))
		})
	})
	When("the child ignores graceful shutdown", func() {
		It("kills and waits for that child then reports the deadline without starting another", func() {
			cfg.Args[len(cfg.Args)-2] = "hang"
			cfg.RestartInterval = 80 * time.Millisecond
			cfg.ShutdownTimeout = 150 * time.Millisecond
			runner, err := supervisor.New(cfg, logger, supervisor.WithOutput(io.Discard, io.Discard))
			Expect(err).NotTo(HaveOccurred())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = runner.Run(ctx)
			Expect(errors.Is(err, supervisor.ErrShutdownTimeout)).To(BeTrue())
			Expect(events(path)).To(Equal([]string{"start", "eof"}))
			// The parent must not delete a possibly stale application lock after Kill.
			_, err = os.Stat(path + ".active")
			Expect(err).NotTo(HaveOccurred())
		})
	})
	When("the executable cannot start", func() {
		It("returns the startup failure without retrying", func() {
			cfg.Executable = filepath.Join(GinkgoT().TempDir(), "missing-program")
			runner, err := supervisor.New(cfg, logger, supervisor.WithOutput(io.Discard, io.Discard))
			Expect(err).NotTo(HaveOccurred())
			Expect(runner.Run(context.Background())).To(HaveOccurred())
			Expect(events(path)).To(BeEmpty())
		})
	})
	When("constructor inputs are invalid", func() {
		It("requires an executable, logger and positive timing bounds", func() {
			for _, change := range []func(*supervisor.Config){func(c *supervisor.Config) { c.Executable = "" }, func(c *supervisor.Config) { c.RestartInterval = 0 }, func(c *supervisor.Config) { c.RestartInterval = -time.Second }, func(c *supervisor.Config) { c.ShutdownTimeout = 0 }, func(c *supervisor.Config) { c.ShutdownTimeout = -time.Second }} {
				invalid := cfg
				change(&invalid)
				_, err := supervisor.New(invalid, logger)
				Expect(err).To(MatchError(supervisor.ErrConfiguration))
			}
			_, err := supervisor.New(cfg, nil)
			Expect(err).To(MatchError(supervisor.ErrConfiguration))
			_, err = supervisor.New(cfg, logger, nil)
			Expect(err).To(MatchError(supervisor.ErrConfiguration))
			_, err = supervisor.New(cfg, logger, supervisor.WithOutput(nil, io.Discard))
			Expect(errors.Is(err, supervisor.ErrConfiguration)).To(BeTrue())
		})
	})
})
