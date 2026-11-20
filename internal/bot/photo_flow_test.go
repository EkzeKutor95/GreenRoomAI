package bot_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/assistant"
	"greenroom-ai/internal/bot"
	"greenroom-ai/internal/telegram"
)

var flowTG, flowAI *httptest.Server
var flowImage []byte
var flowState struct {
	sync.Mutex
	messages                                 []string
	metadataCalls, downloadCalls, modelCalls int
}

var _ = BeforeSuite(func() {
	var encoded bytes.Buffer
	Expect(png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)))).To(Succeed())
	flowImage = encoded.Bytes()
	flowTG = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer GinkgoRecover()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/botfake-telegram-token/sendMessage":
			var body struct {
				Text string `json:"text"`
			}
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			flowState.Lock()
			flowState.messages = append(flowState.messages, body.Text)
			flowState.Unlock()
			Expect(json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})).To(Succeed())
		case "/botfake-telegram-token/getFile":
			var body struct {
				FileID string `json:"file_id"`
			}
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			Expect(body.FileID).To(Equal("photo-large"))
			flowState.Lock()
			flowState.metadataCalls++
			flowState.Unlock()
			Expect(json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": body.FileID, "file_path": "photos/synthetic.png", "file_size": len(flowImage)}})).To(Succeed())
		case "/file/botfake-telegram-token/photos/synthetic.png":
			Expect(r.Method).To(Equal(http.MethodGet))
			w.Header().Set("Content-Type", "image/png")
			flowState.Lock()
			flowState.downloadCalls++
			flowState.Unlock()
			_, err := w.Write(flowImage)
			Expect(err).NotTo(HaveOccurred())
		default:
			Fail("unexpected Telegram route")
		}
	}))
	flowAI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer GinkgoRecover()
		Expect(r.URL.Path).To(Equal("/responses"))
		Expect(r.Header.Get("Authorization")).To(Equal("Bearer fake-openai-key"))
		var body struct {
			Store bool `json:"store"`
			Input []struct {
				Content []struct {
					Type     string `json:"type"`
					ImageURL string `json:"image_url"`
				} `json:"content"`
			} `json:"input"`
		}
		Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
		Expect(body.Store).To(BeFalse())
		Expect(body.Input).To(HaveLen(1))
		Expect(body.Input[0].Content).To(HaveLen(2))
		dataURL := body.Input[0].Content[1].ImageURL
		Expect(dataURL).To(HavePrefix("data:image/png;base64,"))
		Expect(dataURL).NotTo(ContainSubstring("fake-telegram-token"))
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, "data:image/png;base64,"))
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(Equal(flowImage))
		flowState.Lock()
		flowState.modelCalls++
		flowState.Unlock()
		transcript := `{"lines":[{"text":"Тестовая строка","uncertain":false,"kind":"handwritten"},{"text":"[неразборчиво]","uncertain":true,"kind":"handwritten"}],"unreadable":false,"warnings":["handwriting_uncertain"]}`
		w.Header().Set("Content-Type", "application/json")
		Expect(json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": transcript}}}}})).To(Succeed())
	}))
})
var _ = AfterSuite(func() { flowTG.Close(); flowAI.Close() })

var _ = Describe("Complete photo recognition through HTTP adapters", func() {
	It("downloads authorized media, submits inline pixels and returns the draft without invoking financial reports", func() {
		flowState.Lock()
		flowState.messages = nil
		flowState.metadataCalls = 0
		flowState.downloadCalls = 0
		flowState.modelCalls = 0
		flowState.Unlock()
		tg, err := telegram.New(telegram.Config{Token: "fake-telegram-token", BaseURL: flowTG.URL}, flowTG.Client())
		Expect(err).NotTo(HaveOccurred())
		source := &data{}
		ai, err := assistant.New(assistant.Config{Key: "fake-openai-key", Model: "configured-vision-model", BaseURL: flowAI.URL, Location: time.UTC}, flowAI.Client(), source)
		Expect(err).NotTo(HaveOccurred())
		app, err := bot.New(bot.Config{AllowedUserIDs: []int64{12}, Location: time.UTC}, tg, source, &settings{}, slog.New(slog.NewTextHandler(io.Discard, nil)), bot.WithPhotoRecognition(tg, ai))
		Expect(err).NotTo(HaveOccurred())
		Expect(app.Handle(context.Background(), photoUpdate(12))).To(Succeed())
		flowState.Lock()
		defer flowState.Unlock()
		Expect(flowState.metadataCalls).To(Equal(1))
		Expect(flowState.downloadCalls).To(Equal(1))
		Expect(flowState.modelCalls).To(Equal(1))
		Expect(flowState.messages).To(HaveLen(2))
		result := flowState.messages[1]
		Expect(result).To(ContainSubstring("Тестовая строка\n[неразборчиво]"))
		Expect(result).To(ContainSubstring("черновик"))
		Expect(result).NotTo(ContainSubstring("Черновик распознавания фото"))
		Expect(source.calls).To(BeZero())
	})
})
