package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func assertInFlight(t *testing.T, limiter *RequestLimiter, want int) {
	t.Helper()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.inFlight != want {
		t.Fatalf("in flight=%d want=%d", limiter.inFlight, want)
	}
}

func TestInFlightResponseLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := &RequestLimiter{}
		if err := limiter.configure(0, 0, 2); err != nil {
			t.Fatal(err)
		}
		var calls int
		transport := rateLimitedTransport{limiter: limiter, base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})}
		request := func(ctx context.Context) *http.Response {
			t.Helper()
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://example.invalid", nil)
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return nil
			}
			return resp
		}
		first := request(t.Context())
		second := request(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		third := make(chan *http.Response, 1)
		go func() { third <- request(ctx) }()
		synctest.Wait()
		if calls != 2 {
			t.Fatalf("released at headers: calls=%d", calls)
		}
		assertInFlight(t, limiter, 2)
		if _, err := io.ReadAll(first.Body); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		thirdResponse := <-third
		fourth := make(chan *http.Response, 1)
		go func() { fourth <- request(t.Context()) }()
		_ = first.Body.Close() // EOF + repeated Close must not return another slot.
		_ = first.Body.Close()
		synctest.Wait()
		if calls != 3 {
			t.Fatalf("double release: calls=%d", calls)
		}
		cancel() // The caller need not read the body again for cancellation to release it.
		synctest.Wait()
		fourthResponse := <-fourth
		_ = thirdResponse.Body.Close()
		assertInFlight(t, limiter, 2)
		_ = second.Body.Close()
		_ = fourthResponse.Body.Close()
		assertInFlight(t, limiter, 0)
	})
}

func TestInFlightCancelledWaitersAndReconfiguration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := &RequestLimiter{}
		if err := limiter.configure(0, 0, 3); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if err := limiter.acquire(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		cancels := make([]context.CancelFunc, 100)
		for i := range cancels {
			ctx, cancel := context.WithCancel(t.Context())
			cancels[i] = cancel
			wg.Go(func() {
				if err := limiter.acquire(ctx); !errors.Is(err, context.Canceled) {
					t.Errorf("waiter=%v", err)
				}
			})
		}
		synctest.Wait()
		for _, cancel := range cancels {
			cancel()
		}
		wg.Wait()
		assertInFlight(t, limiter, 3)
		if len(limiter.sent) != 3 {
			t.Fatal("cancelled waiters consumed rate quota")
		}
		if err := limiter.configure(0, 0, 1); err != nil {
			t.Fatal(err)
		}
		admitted := make(chan struct{})
		go func() {
			if err := limiter.acquire(t.Context()); err != nil {
				t.Error(err)
			}
			close(admitted)
		}()
		limiter.release()
		limiter.release()
		synctest.Wait()
		select {
		case <-admitted:
			t.Fatal("lowered limit ignored active requests")
		default:
		}
		limiter.release()
		synctest.Wait()
		<-admitted
		assertInFlight(t, limiter, 1)
		// Provider rebuilds retain the same object and cannot reset its active count.
		if err := limiter.configure(0, 0, 1); err != nil {
			t.Fatal(err)
		}
		waiting := make(chan struct{})
		go func() {
			if err := limiter.acquire(t.Context()); err != nil {
				t.Error(err)
			}
			close(waiting)
		}()
		synctest.Wait()
		select {
		case <-waiting:
			t.Fatal("reconfiguration reset active slots")
		default:
		}
		if err := limiter.configure(0, 0, 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		<-waiting
		assertInFlight(t, limiter, 2)
		limiter.release()
		limiter.release()
		if err := limiter.configure(0, 0, -1); err == nil {
			t.Fatal("negative limit accepted")
		}
	})
}

func TestInFlightWaitsForRateWithoutHoldingSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := &RequestLimiter{}
		if err := limiter.configure(1, 0, 1); err != nil {
			t.Fatal(err)
		}
		if err := limiter.acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		limiter.release()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- limiter.acquire(ctx) }()
		synctest.Wait()
		assertInFlight(t, limiter, 0)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(limiter.sent) != 1 {
			t.Fatal("rate waiter counted before admission")
		}
	})
}

type inflightErrorBody struct{}

func (inflightErrorBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (inflightErrorBody) Close() error             { return errors.New("close fixture") }

func TestInFlightTransportFailureReleasesSlot(t *testing.T) {
	for _, scenario := range []string{"transport", "nil_response", "nil_body", "no_body", "read_error", "close_error"} {
		t.Run(scenario, func(t *testing.T) {
			limiter := &RequestLimiter{}
			if err := limiter.configure(0, 0, 1); err != nil {
				t.Fatal(err)
			}
			transport := rateLimitedTransport{limiter: limiter, base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				switch scenario {
				case "transport":
					return nil, errors.New("transport fixture")
				case "nil_response":
					return nil, nil
				case "nil_body":
					return &http.Response{StatusCode: 200}, nil
				case "no_body":
					return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
				default:
					return &http.Response{StatusCode: 200, Body: inflightErrorBody{}}, nil
				}
			})}
			req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://example.invalid", nil)
			resp, _ := transport.RoundTrip(req)
			if scenario == "read_error" {
				_, _ = io.ReadAll(resp.Body)
			}
			if scenario == "close_error" || scenario == "read_error" {
				_ = resp.Body.Close()
				_ = resp.Body.Close()
			}
			assertInFlight(t, limiter, 0)
			if err := limiter.acquire(t.Context()); err != nil {
				t.Fatal(err)
			}
			limiter.release()
		})
	}
}

func TestProviderInFlightLimitAllFormats(t *testing.T) {
	for _, format := range []string{"openai", "anthropic", "openai-responses"} {
		for _, stream := range []bool{false, true} {
			for _, retry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream_%t/retry_%t", format, stream, retry), func(t *testing.T) {
					var active, peak, calls, rejected atomic.Int64
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						call := calls.Add(1)
						n := active.Add(1)
						defer active.Add(-1)
						for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
						}
						if n > 3 {
							rejected.Add(1)
							w.WriteHeader(429)
							return
						}
						if retry && call == 1 {
							w.WriteHeader(500)
							_, _ = io.WriteString(w, `{"error":{"message":"retry fixture"}}`)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						w.WriteHeader(200)
						w.(http.Flusher).Flush() // Headers must not return a concurrency slot.
						time.Sleep(25 * time.Millisecond)
						_, _ = io.WriteString(w, inFlightFixture(format, stream))
					}))
					defer upstream.Close()
					cfg := ConfigFrom(format, "fixture", upstream.URL, "test-key", "")
					cfg.MaxConcurrency = 3
					cfg.Retry.ConnectAttempts, cfg.Retry.EmptyAttempts = -1, -1
					if retry {
						cfg.Retry.ConnectAttempts = 1
						cfg.Retry.ConnectInterval = time.Millisecond
					}
					limiter := &RequestLimiter{}
					providers := make([]llm.Provider, 2)
					for i := range providers {
						var err error
						providers[i], err = cfg.NewProviderWithLimiter(limiter)
						if err != nil {
							t.Fatal(err)
						}
					}
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					var wg sync.WaitGroup
					for i := range 10 {
						wg.Go(func() {
							request := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}}
							var err error
							if stream {
								for _, e := range providers[i%2].Stream(ctx, request) {
									if e != nil {
										err = e
									}
								}
							} else {
								_, _, _, err = providers[i%2].Complete(ctx, request)
							}
							if err != nil {
								t.Error(err)
							}
						})
					}
					wg.Wait()
					wantCalls := int64(10)
					if retry {
						wantCalls++
					}
					if rejected.Load() != 0 || peak.Load() > 3 || peak.Load() < 2 || calls.Load() != wantCalls {
						t.Fatalf("peak=%d rejected=%d calls=%d want=%d", peak.Load(), rejected.Load(), calls.Load(), wantCalls)
					}
					assertInFlight(t, limiter, 0)
				})
			}
		}
	}
}

func inFlightFixture(format string, stream bool) string {
	switch format {
	case "anthropic":
		if stream {
			return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		}
		return `{"id":"m","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	case "openai-responses":
		body := `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
		if stream {
			return "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
		}
		return body
	default:
		if stream {
			return "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		}
		return `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	}
}
