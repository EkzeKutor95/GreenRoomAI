package telegram_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/telegram"
)

var _ = Describe("Telegram delivery", func() {
	It("preserves Unicode text across all bounded message parts", func() {
		original := strings.Repeat("☕🍰 зелёная комната\n", 600)
		chunks := telegram.Chunks(original, 3500)
		Expect(strings.Join(chunks, "")).To(Equal(original))
		Expect(len(chunks)).To(BeNumerically(">", 1))
		for _, chunk := range chunks {
			Expect(len(utf16.Encode([]rune(chunk)))).To(BeNumerically("<=", 3500))
		}
	})
	It("acknowledges polling offsets and restricts update types", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			Expect(r.URL.Path).To(Equal("/bottest-token/getUpdates"))
			var payload map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&payload)).To(Succeed())
			Expect(payload["offset"]).To(BeNumerically("==", 43))
			Expect(payload["allowed_updates"]).To(ConsistOf("message", "callback_query"))
			_, err := io.WriteString(w, `{"ok":true,"result":[{"update_id":43,"message":{"from":{"id":12},"chat":{"id":12,"type":"private"},"text":"/start"}}]}`)
			Expect(err).NotTo(HaveOccurred())
		}))
		defer server.Close()
		client, err := telegram.New(telegram.Config{Token: "test-token", BaseURL: server.URL}, server.Client())
		Expect(err).NotTo(HaveOccurred())
		updates, err := client.Updates(context.Background(), 43)
		Expect(err).NotTo(HaveOccurred())
		Expect(updates).To(HaveLen(1))
		Expect(updates[0].Message.Text).To(Equal("/start"))
	})
	It("keeps request tokens and server error bodies out of errors", func() {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			requests.Add(1)
			w.WriteHeader(429)
			_, err := io.WriteString(w, `{"ok":false,"error_code":429,"description":"SECRET-TOKEN","parameters":{"retry_after":7}}`)
			Expect(err).NotTo(HaveOccurred())
		}))
		defer server.Close()
		client, err := telegram.New(telegram.Config{Token: "SECRET-TOKEN", BaseURL: server.URL}, server.Client())
		Expect(err).NotTo(HaveOccurred())
		err = client.Send(context.Background(), 12, "business report", nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("SECRET"))
		Expect(requests.Load()).To(Equal(int32(1)))
		var apiError *telegram.APIError
		Expect(errors.As(err, &apiError)).To(BeTrue())
		Expect(apiError.RetryAfter).To(Equal(7 * time.Second))
	})
	It("sends the full CSV as a document with a protected recipient", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			Expect(r.URL.Path).To(HaveSuffix("/sendDocument"))
			Expect(r.ParseMultipartForm(1 << 20)).To(Succeed())
			Expect(r.FormValue("chat_id")).To(Equal("12"))
			Expect(r.FormValue("protect_content")).To(Equal("true"))
			file, header, err := r.FormFile("document")
			Expect(err).NotTo(HaveOccurred())
			defer file.Close()
			Expect(header.Filename).To(Equal("report.csv"))
			data, err := io.ReadAll(file)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(Equal("position;revenue\ncoffee;100\n"))
			_, err = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
			Expect(err).NotTo(HaveOccurred())
		}))
		defer server.Close()
		client, err := telegram.New(telegram.Config{Token: "test", BaseURL: server.URL}, server.Client())
		Expect(err).NotTo(HaveOccurred())
		Expect(client.Document(context.Background(), 12, "report.csv", []byte("position;revenue\ncoffee;100\n"))).To(Succeed())
	})
	It("refuses redirects instead of disclosing token paths", func() {
		var redirected atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			redirected.Store(true)
		}))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer server.Close()
		client, err := telegram.New(telegram.Config{Token: "test", BaseURL: server.URL}, server.Client())
		Expect(err).NotTo(HaveOccurred())
		Expect(client.Check(context.Background())).NotTo(Succeed())
		Expect(redirected.Load()).To(BeFalse())
	})
})
