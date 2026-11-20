package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Managed worker shutdown", func() {
	When("the supervisor closes its control pipe", func() {
		It("cancels the worker so its normal cleanup can finish", func() {
			reader, writer := io.Pipe()
			DeferCleanup(func() { Expect(reader.Close()).To(Succeed()) })
			ctx := contextFromControlPipe(context.Background(), reader)

			Expect(writer.Close()).To(Succeed())

			Eventually(ctx.Done()).Should(BeClosed())
			Expect(context.Cause(ctx)).To(MatchError(context.Canceled))
		})
	})

	When("the control pipe remains open", func() {
		It("keeps the worker running until EOF rather than treating data as a restart", func() {
			reader, writer := io.Pipe()
			DeferCleanup(func() { Expect(reader.Close()).To(Succeed()) })
			DeferCleanup(func() { Expect(writer.Close()).To(Succeed()) })
			ctx := contextFromControlPipe(context.Background(), reader)

			_, err := io.Copy(writer, strings.NewReader("still open"))
			Expect(err).NotTo(HaveOccurred())

			Consistently(ctx.Done(), 30*time.Millisecond).ShouldNot(BeClosed())
			Expect(writer.Close()).To(Succeed())
			Eventually(ctx.Done()).Should(BeClosed())
		})
	})

	When("reading the control pipe fails", func() {
		It("preserves the infrastructure failure as the cancellation cause", func() {
			failure := errors.New("synthetic pipe failure")
			ctx := contextFromControlPipe(context.Background(), failingControlReader{err: failure})

			Eventually(ctx.Done()).Should(BeClosed())

			Expect(errors.Is(context.Cause(ctx), failure)).To(BeTrue())
			Expect(context.Cause(ctx).Error()).To(ContainSubstring("read supervisor control pipe"))
		})
	})

	When("the worker receives a termination signal before EOF", func() {
		It("preserves parent cancellation without waiting for more pipe input", func() {
			parent, stop := context.WithCancel(context.Background())
			DeferCleanup(stop)
			reader, writer := io.Pipe()
			DeferCleanup(func() { Expect(reader.Close()).To(Succeed()) })
			DeferCleanup(func() { Expect(writer.Close()).To(Succeed()) })
			ctx := contextFromControlPipe(parent, reader)

			stop()

			Eventually(ctx.Done()).Should(BeClosed())
			Expect(context.Cause(ctx)).To(MatchError(context.Canceled))
		})
	})
})

type failingControlReader struct{ err error }

func (r failingControlReader) Read([]byte) (int, error) { return 0, r.err }
