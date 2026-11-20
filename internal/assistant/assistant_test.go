package assistant_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/assistant"
)

type reportsStub struct {
	calls  []analytics.Period
	err    error
	byDate map[string]analytics.Dataset
}

func (s *reportsStub) Sales(_ context.Context, p analytics.Period) (analytics.Dataset, error) {
	s.calls = append(s.calls, p)
	if s.err != nil {
		return analytics.Dataset{}, s.err
	}
	return s.byDate[p.From.Format("2006-01-02")], nil
}
func financial(value string) analytics.Decimal {
	v, err := analytics.ParseDecimal(value)
	Expect(err).NotTo(HaveOccurred())
	return v
}
func dataset(date, revenue string) analytics.Dataset {
	count := 1
	return analytics.Dataset{ReceiptCount: &count, Rows: []analytics.Row{{Date: date, Group: "Напитки", Name: "Кофе", Quantity: financial("0.5"), Revenue: financial(revenue)}}}
}
func tool(name, args string) any {
	return map[string]any{"status": "completed", "output": []any{map[string]any{"type": "function_call", "name": name, "call_id": "call", "arguments": args}}}
}
func message(text string) any {
	return map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}
}

type responseScript struct {
	mu             sync.Mutex
	queue          []any
	requests       []map[string]any
	status         int
	body, redirect string
	targetCalls    int
}

var server, target *httptest.Server
var script responseScript
var source *reportsStub
var client *assistant.Client

var _ = BeforeSuite(func() {
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		script.mu.Lock()
		script.targetCalls++
		script.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		script.mu.Lock()
		defer script.mu.Unlock()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		script.requests = append(script.requests, body)
		if script.redirect != "" {
			w.Header().Set("Location", script.redirect)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if script.status != 0 {
			w.WriteHeader(script.status)
			if _, err := io.WriteString(w, script.body); err != nil {
				return
			}
			return
		}
		if len(script.queue) == 0 {
			w.WriteHeader(500)
			return
		}
		out := script.queue[0]
		script.queue = script.queue[1:]
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			return
		}
	}))
})
var _ = AfterSuite(func() { server.Close(); target.Close() })

var _ = Describe("Optional AI interpreter", func() {
	BeforeEach(func() {
		script.mu.Lock()
		script.queue = nil
		script.requests = nil
		script.status = 0
		script.body = ""
		script.redirect = ""
		script.targetCalls = 0
		script.mu.Unlock()
		source = &reportsStub{byDate: map[string]analytics.Dataset{"2026-11-10": dataset("2026-11-10", "200.10"), "2026-11-09": dataset("2026-11-09", "100.05")}}
		var err error
		client, err = assistant.New(assistant.Config{Key: "private-key", Model: "configured-model", BaseURL: server.URL, Location: time.UTC}, server.Client(), source)
		Expect(err).NotTo(HaveOccurred())
	})
	When("a sales question triggers a safe report tool", func() {
		It("returns Go-computed financial values and only predefined qualitative interpretation", func() {
			script.queue = []any{tool("sales_report", `{"start":"2026-11-10","end":"2026-11-10"}`), message(`{"insights":["causes_not_proven","dessert_availability"]}`)}
			answer, err := client.Ask(context.Background(), "Какая выручка за 10 ноября?")
			Expect(err).NotTo(HaveOccurred())
			Expect(answer).To(ContainSubstring("Выручка: 200.10 ₽"))
			Expect(answer).To(ContainSubstring("не устанавливают причину"))
			Expect(answer).To(ContainSubstring("нет идентификатора конкретного заказа"))
			Expect(source.calls).To(HaveLen(1))
			script.mu.Lock()
			requests := append([]map[string]any(nil), script.requests...)
			script.mu.Unlock()
			Expect(requests).To(HaveLen(2))
			Expect(requests[0]["store"]).To(BeFalse())
			Expect(requests[0]["parallel_tool_calls"]).To(BeFalse())
			Expect(requests[0]["text"]).NotTo(BeNil())
		})
	})
	When("the model asks to compare two valid periods", func() {
		It("chooses the revenue trend from exact Go results rather than trusting a model assertion", func() {
			script.queue = []any{tool("compare_periods", `{"start":"2026-11-10","end":"2026-11-10","previous_start":"2026-11-09","previous_end":"2026-11-09"}`), message(`{"insights":["revenue_trend"]}`)}
			answer, err := client.Ask(context.Background(), "Сравни два дня")
			Expect(err).NotTo(HaveOccurred())
			Expect(answer).To(ContainSubstring("выше базового"))
			Expect(answer).To(ContainSubstring("100.00%"))
			Expect(source.calls).To(HaveLen(2))
		})
	})
	When("a model tries to return invented financial prose", func() {
		It("rejects the response including fabricated amounts written as words", func() {
			script.queue = []any{tool("sales_report", `{"start":"2026-11-10","end":"2026-11-10"}`), message("Выручка составила миллион рублей")}
			answer, err := client.Ask(context.Background(), "Продажи")
			Expect(err).To(MatchError(assistant.ErrInvalidInsight))
			Expect(answer).To(BeEmpty())
		})
	})
	When("the model returns an unsupported tool", func() {
		It("rejects it before querying iiko", func() {
			script.queue = []any{tool("change_order", `{"start":"2026-11-10","end":"2026-11-10"}`)}
			_, err := client.Ask(context.Background(), "Измени заказ")
			Expect(errors.Is(err, assistant.ErrInvalidTool)).To(BeTrue())
			Expect(source.calls).To(BeEmpty())
		})
	})
	When("a comparison has an invalid second range", func() {
		It("validates the entire call before any external reads", func() {
			script.queue = []any{tool("compare_periods", `{"start":"2026-11-10","end":"2026-11-10","previous_start":"not-a-date","previous_end":"2026-11-09"}`)}
			_, err := client.Ask(context.Background(), "Сравни")
			Expect(errors.Is(err, assistant.ErrInvalidTool)).To(BeTrue())
			Expect(source.calls).To(BeEmpty())
		})
	})
	When("tool arguments contain unexpected keys or trailing JSON", func() {
		It("refuses those arguments without doing work", func() {
			for _, args := range []string{`{"start":"2026-11-10","end":"2026-11-10","secret":"no"}`, `{"start":"2026-11-10","end":"2026-11-10"} {}`} {
				script.queue = []any{tool("sales_report", args)}
				_, err := client.Ask(context.Background(), "Отчёт")
				Expect(errors.Is(err, assistant.ErrInvalidTool)).To(BeTrue())
				Expect(source.calls).To(BeEmpty())
			}
		})
	})
	When("iiko is unavailable", func() {
		It("preserves the dependency failure instead of fabricating an empty report", func() {
			failure := errors.New("upstream unavailable")
			source.err = failure
			script.queue = []any{tool("sales_report", `{"start":"2026-11-10","end":"2026-11-10"}`)}
			answer, err := client.Ask(context.Background(), "Отчёт")
			Expect(errors.Is(err, failure)).To(BeTrue())
			Expect(answer).To(BeEmpty())
		})
	})
	When("the OpenAI API denies access with a sensitive response body", func() {
		It("returns a safe status error without echoing the body or key", func() {
			script.status = 403
			script.body = "private-key and personal data"
			_, err := client.Ask(context.Background(), "Отчёт")
			Expect(errors.Is(err, assistant.ErrUnavailable)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("403"))
			Expect(err.Error()).NotTo(ContainSubstring("private-key"))
			Expect(err.Error()).NotTo(ContainSubstring("personal data"))
		})
	})
	When("the OpenAI endpoint redirects a credential-bearing request", func() {
		It("does not follow the redirect", func() {
			script.redirect = target.URL
			_, err := client.Ask(context.Background(), "Отчёт")
			Expect(errors.Is(err, assistant.ErrUnavailable)).To(BeTrue())
			script.mu.Lock()
			calls := script.targetCalls
			script.mu.Unlock()
			Expect(calls).To(Equal(0))
		})
	})
	When("the user asks a staff question", func() {
		It("directs them to the staff menu without sending unrelated business data to the model", func() {
			script.queue = []any{message(`{"insights":["employee_menu"]}`)}
			answer, err := client.Ask(context.Background(), "Какой бариста работал?")
			Expect(err).NotTo(HaveOccurred())
			Expect(answer).To(ContainSubstring("разделе «Сотрудники»"))
			Expect(source.calls).To(BeEmpty())
		})
	})
	When("a model exceeds the bounded tool budget", func() {
		It("stops after three read-only calls", func() {
			call := tool("sales_report", `{"start":"2026-11-10","end":"2026-11-10"}`)
			script.queue = []any{call, call, call, call}
			_, err := client.Ask(context.Background(), "Запроси много отчётов")
			Expect(err).To(MatchError(assistant.ErrToolLimit))
			Expect(source.calls).To(HaveLen(3))
		})
	})
	When("required constructor settings are invalid", func() {
		It("rejects insecure remote HTTP and credentials embedded in URLs", func() {
			for _, base := range []string{"http://example.com/v1", "https://user:password@example.com/v1", "https://example.com/v1?token=secret", "https://example.com/v1#fragment"} {
				_, err := assistant.New(assistant.Config{Key: "key", Model: "model", BaseURL: base, Location: time.UTC}, server.Client(), source)
				Expect(err).To(MatchError(assistant.ErrConfiguration))
			}
		})
	})
})
