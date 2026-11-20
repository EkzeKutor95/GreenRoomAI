// Package state persists only technical cursors, notification keys and settings.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type snapshot struct {
	Version int                  `json:"version"`
	Offset  int64                `json:"telegram_offset"`
	Enabled bool                 `json:"automatic_reports_enabled"`
	Sent    map[string]time.Time `json:"sent"`
}

type Store struct {
	mu   sync.Mutex
	path string
	data snapshot
}

func Open(path string, enabled bool) (*Store, error) {
	if path == "" {
		return nil, ErrInvalid
	}
	s := &Store{path: path, data: snapshot{Version: 1, Enabled: enabled, Sent: map[string]time.Time{}}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open technical state: %w", err)
	}
	defer f.Close() // Read-only file.
	raw, err := io.ReadAll(io.LimitReader(f, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read technical state: %w", err)
	}
	if len(raw) >= 2<<20 {
		return nil, ErrInvalid
	}
	if err = json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("decode technical state: %w", ErrInvalid)
	}
	if s.data.Version != 1 || s.data.Offset < 0 || s.data.Sent == nil {
		return nil, ErrInvalid
	}
	return s, nil
}
func (s *Store) Offset() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.data.Offset }
func (s *Store) SetOffset(offset int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset < s.data.Offset {
		return ErrInvalid
	}
	old := s.data.Offset
	s.data.Offset = offset
	if err := s.write(); err != nil {
		s.data.Offset = old
		return err
	}
	return nil
}
func (s *Store) Enabled() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.data.Enabled }
func (s *Store) SetEnabled(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.data.Enabled
	s.data.Enabled = enabled
	if err := s.write(); err != nil {
		s.data.Enabled = old
		return err
	}
	return nil
}
func (s *Store) Sent(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data.Sent[key]
	return ok
}

// MarkSent retains the marker in memory on persistence failure: the remote send
// already succeeded, so retrying it within this process would create a duplicate.
func (s *Store) MarkSent(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		return ErrInvalid
	}
	now := time.Now().UTC()
	s.data.Sent[key] = now
	for id, at := range s.data.Sent {
		if now.Sub(at) > 90*24*time.Hour {
			delete(s.data.Sent, id)
		}
	}
	return s.write()
}
func (s *Store) write() (result error) {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode technical state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create atomic state file: %w", err)
	}
	name := f.Name()
	closed := false
	defer func() {
		if !closed {
			if err := f.Close(); err != nil {
				result = errors.Join(result, fmt.Errorf("close temporary state: %w", err))
			}
		}
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove temporary state: %w", err))
		}
	}()
	if _, err = f.Write(raw); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync state: %w", err)
	}
	closed = true
	if err = f.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err = os.Rename(name, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

// Acquire prevents simultaneous polling from this installation. A stale lock is
// deliberately not auto-deleted: a guessed PID check can start a second poller.
func Acquire(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, fmt.Errorf("acquire instance lock: %w", err)
	}
	if _, err = f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		return nil, errors.Join(fmt.Errorf("write instance lock: %w", err), f.Close(), removeLock(path))
	}
	if err = f.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("close instance lock: %w", err), removeLock(path))
	}
	return func() error { return removeLock(path) }, nil
}
func removeLock(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove instance lock: %w", err)
	}
	return nil
}
