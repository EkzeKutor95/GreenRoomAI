package state_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/state"
	"os"
	"path/filepath"
)

var _ = Describe("Technical state", func() {
	It("retains delivery markers, settings and the update cursor after restart", func() {
		path := filepath.Join(GinkgoT().TempDir(), "run", "state.json")
		s, err := state.Open(path, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.SetOffset(42)).To(Succeed())
		Expect(s.SetEnabled(true)).To(Succeed())
		Expect(s.MarkSent("notification-1")).To(Succeed())
		restored, err := state.Open(path, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Offset()).To(Equal(int64(42)))
		Expect(restored.Enabled()).To(BeTrue())
		Expect(restored.Sent("notification-1")).To(BeTrue())
	})
	It("rejects corrupt files instead of silently losing deduplication state", func() {
		p := filepath.Join(GinkgoT().TempDir(), "state.json")
		Expect(os.WriteFile(p, []byte("{"), 0600)).To(Succeed())
		_, err := state.Open(p, true)
		Expect(err).To(HaveOccurred())
	})
	It("keeps a successful remote delivery marked in memory when disk persistence fails", func() {
		p := filepath.Join(GinkgoT().TempDir(), "parent")
		Expect(os.WriteFile(p, []byte("file"), 0600)).To(Succeed())
		s, err := state.Open(filepath.Join(p, "state.json"), true)
		Expect(err).To(HaveOccurred())
		p = filepath.Join(GinkgoT().TempDir(), "state.json")
		s, err = state.Open(p, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.Mkdir(p, 0700)).To(Succeed())
		Expect(s.MarkSent("delivered")).NotTo(Succeed())
		Expect(s.Sent("delivered")).To(BeTrue())
	})
	It("prevents two polling instances until the owning process releases the lock", func() {
		p := filepath.Join(GinkgoT().TempDir(), "state.lock")
		release, err := state.Acquire(p)
		Expect(err).NotTo(HaveOccurred())
		_, err = state.Acquire(p)
		Expect(err).To(MatchError(state.ErrAlreadyRunning))
		Expect(release()).To(Succeed())
		release, err = state.Acquire(p)
		Expect(err).NotTo(HaveOccurred())
		Expect(release()).To(Succeed())
	})
})
