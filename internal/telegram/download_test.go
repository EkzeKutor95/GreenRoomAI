package telegram_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/telegram"
)

type canceledReader struct{ cancel context.CancelFunc }

func (r canceledReader) Read([]byte) (int, error) {
	r.cancel()
	return 0, fmt.Errorf("SECRET-TOKEN read failure")
}
func (r canceledReader) Close() error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), ContentLength: int64(len(body))}
}
func metadataResponse(filePath string, size int64) *http.Response {
	data, err := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"file_id": "photo-id", "file_path": filePath, "file_size": size}})
	Expect(err).NotTo(HaveOccurred())
	return mockResponse(string(data))
}
func mockDownloadClient(transport roundTripFunc) *telegram.Client {
	client, err := telegram.New(telegram.Config{Token: "SECRET-TOKEN"}, &http.Client{Transport: transport})
	Expect(err).NotTo(HaveOccurred())
	return client
}

var _ = Describe("Telegram photo and document input", func() {
	It("decodes photo sizes and caption without treating a photo as an empty message", func() {
		var update telegram.Update
		err := json.Unmarshal([]byte(`{"update_id":44,"message":{"message_id":7,"from":{"id":12},"chat":{"id":12,"type":"private"},"caption":"Отчёт за смену","photo":[{"file_id":"small","file_size":12,"width":90,"height":80},{"file_id":"large","file_size":12345,"width":1200,"height":900}]}}`), &update)
		Expect(err).NotTo(HaveOccurred())
		Expect(update.Message.Caption).To(Equal("Отчёт за смену"))
		Expect(update.Message.Photo).To(HaveLen(2))
		Expect(update.Message.Photo[1]).To(Equal(telegram.PhotoSize{FileID: "large", FileSize: 12345, Width: 1200, Height: 900}))
		Expect(update.Message.Document).To(BeNil())
	})
	It("decodes image document metadata with a 64-bit size", func() {
		var update telegram.Update
		err := json.Unmarshal([]byte(`{"update_id":45,"message":{"from":{"id":12},"chat":{"id":12,"type":"private"},"caption":"Снимок","document":{"file_id":"document-id","file_name":"shift.png","mime_type":"image/png","file_size":4294967296}}}`), &update)
		Expect(err).NotTo(HaveOccurred())
		Expect(update.Message.Document).To(Equal(&telegram.FileDocument{FileID: "document-id", FileName: "shift.png", MIMEType: "image/png", FileSize: 4294967296}))
		Expect(update.Message.Photo).To(BeEmpty())
	})
})

var _ = Describe("Bounded private file downloads", func() {
	It("resolves a file identifier and downloads only its same-origin Telegram path", func() {
		var requests atomic.Int32
		body := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			requests.Add(1)
			switch r.URL.Path {
			case "/botSECRET-TOKEN/getFile":
				Expect(r.Method).To(Equal(http.MethodPost))
				var payload map[string]string
				Expect(json.NewDecoder(r.Body).Decode(&payload)).To(Succeed())
				Expect(payload).To(Equal(map[string]string{"file_id": "photo-id"}))
				Expect(json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "photo-id", "file_size": len(body), "file_path": "photos/file_8.png"}})).To(Succeed())
			case "/file/botSECRET-TOKEN/photos/file_8.png":
				Expect(r.Method).To(Equal(http.MethodGet))
				Expect(r.URL.RawQuery).To(BeEmpty())
				_, err := w.Write(body)
				Expect(err).NotTo(HaveOccurred())
			default:
				Fail("unexpected file endpoint")
			}
		}))
		defer server.Close()
		client, err := telegram.New(telegram.Config{Token: "SECRET-TOKEN", BaseURL: server.URL}, server.Client())
		Expect(err).NotTo(HaveOccurred())
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(body))
		Expect(requests.Load()).To(Equal(int32(2)))
	})
	DescribeTable("rejects unsafe paths before any GET request", func(filePath string) {
		calls := 0
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			calls++
			Expect(r.Method).To(Equal(http.MethodPost))
			return metadataResponse(filePath, 0), nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).To(MatchError(telegram.ErrFilePath))
		Expect(result).To(BeNil())
		Expect(calls).To(Equal(1))
		Expect(err.Error()).NotTo(ContainSubstring("SECRET-TOKEN"))
	},
		Entry("missing path", ""), Entry("absolute path", "/photos/file.png"), Entry("foreign authority", "//evil.invalid/file.png"), Entry("foreign URL", "https://evil.invalid/file.png"),
		Entry("parent path", "../file.png"), Entry("nested parent path", "photos/../file.png"), Entry("current directory", "./photos/file.png"), Entry("duplicate separator", "photos//file.png"), Entry("trailing separator", "photos/file.png/"),
		Entry("encoded separator", "photos%2ffile.png"), Entry("double encoded traversal", "photos/%252e%252e/file.png"), Entry("encoded parent", "photos/%2e%2e/file.png"), Entry("encoded backslash", "photos/%5cfile.png"),
		Entry("backslash", "photos\\file.png"), Entry("query", "photos/file.png?token=secret"), Entry("fragment", "photos/file.png#secret"), Entry("Windows path", "C:/private/file.png"), Entry("control byte", "photos/file\n.png"), Entry("dot directory", "."), Entry("parent directory", ".."),
	)
	DescribeTable("rejects invalid identifiers without making a metadata request", func(fileID string) {
		calls := 0
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("must not request") })
		result, err := client.DownloadFile(context.Background(), fileID)
		Expect(err).To(MatchError(telegram.ErrFileID))
		Expect(result).To(BeNil())
		Expect(calls).To(BeZero())
	}, Entry("empty identifier", ""), Entry("blank identifier", " "), Entry("control character", "photo\nsecret"))
	It("refuses an oversized metadata size before downloading bytes", func() {
		calls := 0
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			calls++
			return metadataResponse("photos/file.png", telegram.MaxDownloadBytes+1), nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).To(MatchError(telegram.ErrFileTooLarge))
		Expect(result).To(BeNil())
		Expect(calls).To(Equal(1))
	})
	It("rejects a negative metadata size instead of silently treating it as unknown", func() {
		calls := 0
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			calls++
			return metadataResponse("photos/file.png", -1), nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).To(MatchError(ContainSubstring(telegram.ErrResponse.Error())))
		Expect(result).To(BeNil())
		Expect(calls).To(Equal(1))
	})
	It("enforces the actual stream limit when file_size and Content-Length are unknown", func() {
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				return metadataResponse("photos/file.png", 0), nil
			}
			return &http.Response{StatusCode: 200, ContentLength: -1, Body: io.NopCloser(bytes.NewReader(make([]byte, telegram.MaxDownloadBytes+1))), Header: make(http.Header)}, nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).To(MatchError(telegram.ErrFileTooLarge))
		Expect(result).To(BeNil())
	})
	It("rejects a Content-Length larger than the limit before reading", func() {
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				return metadataResponse("photos/file.png", 0), nil
			}
			return &http.Response{StatusCode: 200, ContentLength: telegram.MaxDownloadBytes + 1, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).To(MatchError(telegram.ErrFileTooLarge))
		Expect(result).To(BeNil())
	})
	It("rejects a truncated file instead of returning partial attachment data", func() {
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				return metadataResponse("photos/file.png", 10), nil
			}
			return mockResponse("short"), nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(errors.Is(err, telegram.ErrResponse)).To(BeTrue())
		Expect(result).To(BeNil())
	})
	DescribeTable("preserves status codes without exposing error bodies or credential URLs", func(metadata bool, status int) {
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost && !metadata {
				return metadataResponse("photos/file.png", 0), nil
			}
			response := mockResponse(`{"ok":false,"description":"SECRET-TOKEN and private attachment","error_code":` + fmt.Sprint(status) + `}`)
			response.StatusCode = status
			return response, nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).To(HaveOccurred())
		Expect(result).To(BeNil())
		Expect(err.Error()).NotTo(ContainSubstring("SECRET-TOKEN"))
		Expect(err.Error()).NotTo(ContainSubstring("private attachment"))
		Expect(err.Error()).NotTo(ContainSubstring("http"))
		var apiError *telegram.APIError
		Expect(errors.As(err, &apiError)).To(BeTrue())
		Expect(apiError.Code).To(Equal(status))
	}, Entry("unauthorized metadata", true, 401), Entry("unavailable metadata", true, 503), Entry("unauthorized download", false, 401), Entry("unavailable download", false, 503))
	It("does not follow a file redirect to another host", func() {
		var redirected atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Store(true) }))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			if r.Method == http.MethodPost {
				Expect(json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "photo-id", "file_path": "photos/file.png"}})).To(Succeed())
				return
			}
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer server.Close()
		client, err := telegram.New(telegram.Config{Token: "SECRET-TOKEN", BaseURL: server.URL}, server.Client())
		Expect(err).NotTo(HaveOccurred())
		_, err = client.DownloadFile(context.Background(), "photo-id")
		Expect(err).To(HaveOccurred())
		Expect(redirected.Load()).To(BeFalse())
		Expect(err.Error()).NotTo(ContainSubstring("SECRET-TOKEN"))
	})
	It("propagates cancellation while preventing URL-bearing transport error leaks", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				return metadataResponse("photos/file.png", 0), nil
			}
			cancel()
			return nil, fmt.Errorf("SECRET-TOKEN private transport detail")
		})
		result, err := client.DownloadFile(ctx, "photo-id")
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		Expect(result).To(BeNil())
		Expect(err.Error()).NotTo(ContainSubstring("SECRET-TOKEN"))
	})
	It("redacts a download transport error when the caller remains active", func() {
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				return metadataResponse("photos/file.png", 0), nil
			}
			return nil, fmt.Errorf("SECRET-TOKEN private transport detail")
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(errors.Is(err, telegram.ErrTransport)).To(BeTrue())
		Expect(result).To(BeNil())
		Expect(err.Error()).NotTo(ContainSubstring("SECRET-TOKEN"))
	})
	It("caps the combined metadata and download deadline at thirty seconds", func() {
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			Expect(ok).To(BeTrue())
			Expect(time.Until(deadline)).To(BeNumerically("<=", 30*time.Second))
			if r.Method == http.MethodPost {
				return metadataResponse("photos/file.png", 0), nil
			}
			return mockResponse("image"), nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(result)).To(Equal("image"))
	})
	It("accepts an attachment exactly at the configured byte limit", func() {
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				return metadataResponse("photos/file.png", telegram.MaxDownloadBytes), nil
			}
			return &http.Response{StatusCode: 200, ContentLength: telegram.MaxDownloadBytes, Body: io.NopCloser(bytes.NewReader(make([]byte, telegram.MaxDownloadBytes))), Header: make(http.Header)}, nil
		})
		result, err := client.DownloadFile(context.Background(), "photo-id")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(HaveLen(telegram.MaxDownloadBytes))
	})
	It("preserves cancellation when the metadata body is interrupted", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: canceledReader{cancel: cancel}, Header: make(http.Header)}, nil
		})
		result, err := client.DownloadFile(ctx, "photo-id")
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		Expect(result).To(BeNil())
		Expect(err.Error()).NotTo(ContainSubstring("SECRET-TOKEN"))
	})

	DescribeTable("treats file identifiers as opaque strings without numeric or alphabet assumptions", func(fileID string) {
		requests := 0
		client := mockDownloadClient(func(r *http.Request) (*http.Response, error) {
			requests++
			if r.Method == http.MethodPost {
				var payload map[string]string
				Expect(json.NewDecoder(r.Body).Decode(&payload)).To(Succeed())
				Expect(payload["file_id"]).To(Equal(fileID))
				body, err := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"file_id": fileID, "file_path": "photos/file.png", "file_size": 5}})
				Expect(err).NotTo(HaveOccurred())
				return mockResponse(string(body)), nil
			}
			return mockResponse("image"), nil
		})
		result, err := client.DownloadFile(context.Background(), fileID)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(result)).To(Equal("image"))
		Expect(requests).To(Equal(2))
	}, Entry("numeric-looking opaque ID", "-1"), Entry("large numeric-looking opaque ID", "-999999999999999999999999"), Entry("opaque punctuation", ":Ag-_/not-a-path"))

})
