// Package supervisor restarts one child process after a graceful stdin shutdown.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Config struct {
	Executable      string
	Args            []string
	RestartInterval time.Duration
	ShutdownTimeout time.Duration
}

type Option func(*Supervisor) error

type Supervisor struct {
	cfg            Config
	logger         *slog.Logger
	stdout, stderr io.Writer
}

func New(cfg Config, logger *slog.Logger, options ...Option) (*Supervisor, error) {
	if strings.TrimSpace(cfg.Executable) == "" || strings.ContainsRune(cfg.Executable, '\x00') || cfg.RestartInterval <= 0 || cfg.ShutdownTimeout <= 0 || logger == nil {
		return nil, ErrConfiguration
	}
	cfg.Args = append([]string(nil), cfg.Args...)
	s := &Supervisor{cfg: cfg, logger: logger, stdout: os.Stdout, stderr: os.Stderr}
	for _, option := range options {
		if option == nil {
			return nil, ErrConfiguration
		}
		if err := option(s); err != nil {
			return nil, fmt.Errorf("configure supervisor: %w", err)
		}
	}
	return s, nil
}

func WithOutput(stdout, stderr io.Writer) Option {
	return func(s *Supervisor) error {
		if stdout == nil || stderr == nil {
			return ErrConfiguration
		}
		s.stdout = stdout
		s.stderr = stderr
		return nil
	}
}

// Run reserves the child's stdin as a shutdown channel. The child must cancel
// its application context on EOF and return after releasing its instance lock.
// Cancellation is normal termination; a failed child is never retried here.
func (s *Supervisor) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		command := exec.Command(s.cfg.Executable, s.cfg.Args...)
		command.Stdout = s.stdout
		command.Stderr = s.stderr
		// Bound inherited stdout/stderr pipes after exit, including a child that
		// accidentally leaves an output handle open in a descendant process.
		command.WaitDelay = s.cfg.ShutdownTimeout
		control, err := command.StdinPipe()
		if err != nil {
			return fmt.Errorf("create supervised shutdown pipe: %w", err)
		}
		if err = command.Start(); err != nil {
			s.logger.Error("supervised process could not start")
			return errors.Join(fmt.Errorf("start supervised process: %w", err), closeControl(control))
		}
		s.logger.Info("supervised process started", "pid", command.Process.Pid)
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		timer := time.NewTimer(s.cfg.RestartInterval)
		select {
		case err := <-done:
			timer.Stop()
			s.logger.Info("supervised process exited before scheduled restart", "pid", command.Process.Pid)
			return errors.Join(processError(err), closeControl(control))
		case <-ctx.Done():
			timer.Stop()
			s.logger.Info("supervisor is stopping child", "pid", command.Process.Pid)
			return s.stop(command, control, done)
		case <-timer.C:
			s.logger.Info("scheduled process restart", "pid", command.Process.Pid)
			if err = s.stop(command, control, done); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Supervisor) stop(command *exec.Cmd, control io.Closer, done <-chan error) error {
	closeErr := closeControl(control)
	timeout := time.NewTimer(s.cfg.ShutdownTimeout)
	defer timeout.Stop()
	select {
	case err := <-done:
		return errors.Join(closeErr, processError(err))
	case <-timeout.C:
		s.logger.Error("supervised process exceeded shutdown timeout", "pid", command.Process.Pid)
		killErr := command.Process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		// Wait always completes before Run can return or start another child.
		waitErr := <-done
		if killErr != nil {
			killErr = fmt.Errorf("kill timed out supervised process: %w", killErr)
		}
		return errors.Join(ErrShutdownTimeout, closeErr, killErr, processError(waitErr))
	}
}

func closeControl(control io.Closer) error {
	err := control.Close()
	// exec.Cmd.Wait also closes the pipe when the child exits first.
	if err == nil || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return fmt.Errorf("close supervised shutdown pipe: %w", err)
}
func processError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("supervised process exited: %w", err)
}
