package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/notification"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	return f(req)
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

type failingReader struct {
	err error
}

func (r failingReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestNewSession(t *testing.T) {
	t.Parallel()
	s := NewSession(context.Background(), "token", "chat")
	require.NotNil(t, s.client)
	assert.Equal(t, 10*time.Second, s.client.Timeout)
	assert.Equal(t, "token", s.token)
	assert.Equal(t, "chat", s.chatID)
	assert.Zero(t, s.messageID)
	assert.Empty(t, s.message)
}

func TestNewSessionWithTimeout(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"longer", 30 * time.Second, 30 * time.Second},
		{"shorter", 250 * time.Millisecond, 250 * time.Millisecond},
		{"zero", 0, 10 * time.Second},
		{"negative", -time.Second, 10 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				s := NewSessionWithTimeout(ctx, "secret-token", "chat", test.timeout)
				started := time.Now()
				s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					deadline, ok := req.Context().Deadline()
					assert.True(t, ok)
					assert.Equal(t, started.Add(test.want), deadline)
					<-req.Context().Done()
					return nil, req.Context().Err()
				})

				err := s.Notify(ctx, notification.State{Operation: "copy", Phase: "start"})
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.NotContains(t, err.Error(), "secret-token")
				assert.Equal(t, test.want, time.Since(started))
			})
		})
	}
}

func TestSessionDisablesHTTPDumping(t *testing.T) {
	var logs bytes.Buffer
	fs.SetLogger(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { fs.SetLogger(slog.Default().Handler()) })
	globalConfig := fs.GetConfig(context.Background())
	previousLogLevel := globalConfig.LogLevel
	globalConfig.LogLevel = fs.LogLevelDebug
	t.Cleanup(func() { globalConfig.LogLevel = previousLogLevel })

	ctx, ci := fs.AddConfig(context.Background())
	ci.Dump = fs.DumpHeaders | fs.DumpRequests | fs.DumpCurl | fs.DumpErrors
	ci.UserAgent = "notification-tests"
	originalConfig := *ci
	s := NewSession(ctx, "secret-token", "chat")
	assert.Equal(t, originalConfig, *ci, "constructing a session must not modify the caller's config")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, "/botsecret-token/sendMessage", req.URL.Path)
		assert.Equal(t, ci.UserAgent, req.UserAgent())
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	request := func(client *http.Client) {
		req, err := buildSendMessageRequest(ctx, "secret-token", "chat", "text")
		require.NoError(t, err)
		req.URL.Scheme, req.URL.Host = endpoint.Scheme, endpoint.Host
		_, err = doRequest(client, req)
		require.ErrorContains(t, err, "telegram returned status 500")
	}

	request(s.client)
	assert.NotContains(t, logs.String(), "secret-token")
	assert.NotContains(t, logs.String(), "HTTP REQUEST")
	request(fshttp.NewClient(ctx))
	assert.Contains(t, logs.String(), "secret-token", "the caller's HTTP dump settings must remain active")
}

func TestSessionNotify(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var paths []string
	var requests []EditMessageTextRequest
	s := NewSession(ctx, "test-token", "-12345")
	s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body EditMessageTextRequest
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		paths = append(paths, req.URL.Path)
		requests = append(requests, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":42}}`)),
		}, nil
	})

	for i, phase := range []string{"start", "running", "end"} {
		state := notification.State{Operation: "copy", Phase: phase}
		want, err := notification.FormatMessage(state)
		require.NoError(t, err)
		require.NoError(t, s.Notify(ctx, state))
		assert.Equal(t, 42, s.messageID)
		assert.Equal(t, want, s.message)
		require.Len(t, requests, i+1)
		assert.Equal(t, "-12345", requests[i].ChatID)
		assert.Equal(t, want, requests[i].Text)
		if i == 0 {
			assert.Zero(t, requests[i].MessageID)
		} else {
			assert.Equal(t, 42, requests[i].MessageID)
		}
		require.NoError(t, s.Notify(ctx, state))
		assert.Len(t, requests, i+1, "identical successful notifications must be skipped")
	}
	assert.Equal(t, []string{
		"/bottest-token/sendMessage",
		"/bottest-token/editMessageText",
		"/bottest-token/editMessageText",
	}, paths)
}

func TestSessionNotifyRetries(t *testing.T) {
	t.Parallel()
	for _, messageID := range []int{0, 42} {
		t.Run(fmt.Sprintf("messageID=%d", messageID), func(t *testing.T) {
			ctx := context.Background()
			state := notification.State{Operation: "sync", Phase: "running"}
			want, err := notification.FormatMessage(state)
			require.NoError(t, err)
			s := NewSession(ctx, "test-token", "chat")
			s.messageID = messageID
			s.message = "last successful message"
			calls := 0
			s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				method := "sendMessage"
				if messageID != 0 {
					method = "editMessageText"
				}
				assert.Equal(t, "/bottest-token/"+method, req.URL.Path)
				body := `{"ok":false,"error_code":429,"description":"retry later"}`
				if calls > 1 {
					body = `{"ok":true,"result":{"message_id":42}}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})

			require.Error(t, s.Notify(ctx, state))
			assert.Equal(t, messageID, s.messageID)
			assert.Equal(t, "last successful message", s.message)
			require.NoError(t, s.Notify(ctx, state))
			assert.Equal(t, 2, calls)
			assert.Equal(t, 42, s.messageID)
			assert.Equal(t, want, s.message)
			require.NoError(t, s.Notify(ctx, state))
			assert.Equal(t, 2, calls)
		})
	}
}

func TestSessionNotifyInvalidState(t *testing.T) {
	t.Parallel()
	s := NewSession(context.Background(), "test-token", "chat")
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("invalid state must not make a request")
		return nil, errors.New("unexpected request")
	})
	err := s.Notify(context.Background(), notification.State{Phase: "invalid"})
	require.ErrorContains(t, err, "unknown state: invalid")
	assert.Zero(t, s.messageID)
	assert.Empty(t, s.message)
}

func TestSessionNotifyCanceled(t *testing.T) {
	t.Parallel()
	for _, messageID := range []int{0, 42} {
		t.Run(fmt.Sprintf("messageID=%d", messageID), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := NewSession(ctx, "secret-token", "chat")
			s.messageID = messageID
			s.message = "last successful message"
			started := make(chan struct{})
			s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-req.Context().Done()
				return nil, req.Context().Err()
			})
			done := make(chan error, 1)
			go func() {
				done <- s.Notify(ctx, notification.State{Operation: "copy", Phase: "running"})
			}()

			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("notification request did not start")
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
				assert.NotContains(t, err.Error(), "secret-token")
			case <-time.After(5 * time.Second):
				t.Fatal("notification request did not stop after cancellation")
			}
			assert.Equal(t, messageID, s.messageID)
			assert.Equal(t, "last successful message", s.message)
		})
	}
}

func TestBuildRequests(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		build func(context.Context, string, string, string) (*http.Request, error)
		id    int
	}{
		{name: "sendMessage", build: buildSendMessageRequest},
		{name: "editMessageText", id: 42, build: func(ctx context.Context, token, chatID, text string) (*http.Request, error) {
			return buildEditMessageTextRequest(ctx, token, chatID, text, 42)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			chatID := "chat\"\\\n"
			message := "Строка \"text\"\n\\ <>&\t🙂"
			req, err := test.build(ctx, "test-token", chatID, message)
			require.NoError(t, err)
			defer req.Body.Close()
			assert.Equal(t, http.MethodPost, req.Method)
			assert.Equal(t, "https://api.telegram.org/bottest-token/"+test.name, req.URL.String())
			assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
			assert.Same(t, ctx, req.Context())
			var body map[string]any
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
			want := map[string]any{"chat_id": chatID, "text": message}
			if test.id != 0 {
				want["message_id"] = float64(test.id)
			}
			assert.Equal(t, want, body)
			cancel()
			assert.ErrorIs(t, req.Context().Err(), context.Canceled)

			_, err = test.build(ctx, "secret-token\n", chatID, message)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret-token")
			_, err = test.build(nil, "secret-token", chatID, message)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret-token")
		})
	}
}

func TestDoRequest(t *testing.T) {
	t.Parallel()
	readErr := errors.New("response read failed")
	for _, test := range []struct {
		name   string
		status int
		reader io.Reader
		want   string
		err    string
	}{
		{name: "success", status: 200, reader: strings.NewReader("response"), want: "response"},
		{name: "last successful status", status: 299, reader: strings.NewReader("response"), want: "response"},
		{name: "below successful status", status: 199, reader: strings.NewReader("failure"), want: "failure", err: "telegram returned status 199"},
		{name: "above successful status", status: 300, reader: strings.NewReader("failure"), want: "failure", err: "telegram returned status 300"},
		{name: "server error", status: 500, reader: strings.NewReader("server failure"), want: "server failure", err: "telegram returned status 500"},
		{name: "read error", status: 200, reader: failingReader{err: readErr}, err: "failed to read telegram response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{Reader: test.reader}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.status,
					Status:     fmt.Sprintf("%d %s", test.status, http.StatusText(test.status)),
					Body:       body,
				}, nil
			})}
			req, err := buildSendMessageRequest(context.Background(), "test-token", "chat", "text")
			require.NoError(t, err)
			got, err := doRequest(client, req)
			assert.True(t, body.closed)
			assert.Equal(t, test.want, string(got))
			if test.err == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.err)
				if test.name == "read error" {
					assert.ErrorIs(t, err, readErr)
				} else {
					assert.Contains(t, err.Error(), test.want)
				}
			}
		})
	}
}

func TestSendMessageResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		body string
		id   int
		err  string
	}{
		{name: "success", body: `{"ok":true,"result":{"message_id":42}}`, id: 42},
		{name: "malformed JSON", body: `{`, err: "failed unmarshal parse telegram response"},
		{name: "API error", body: `{"ok":false,"error_code":403,"description":"forbidden"}`, err: "telegram response is not ok"},
		{name: "missing result", body: `{"ok":true}`, err: "does not contain result"},
		{name: "null result", body: `{"ok":true,"result":null}`, err: "does not contain result"},
		{name: "missing message ID", body: `{"ok":true,"result":{}}`, err: "does not contain message_id"},
		{name: "zero message ID", body: `{"ok":true,"result":{"message_id":0}}`, err: "does not contain message_id"},
		{name: "wrong message ID type", body: `{"ok":true,"result":{"message_id":"42"}}`, err: "failed unmarshal parse telegram response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(test.body)}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
			})}
			id, err := sendMessage(context.Background(), client, "test-token", "chat", "text")
			assert.True(t, body.closed)
			assert.Equal(t, test.id, id)
			if test.err == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.err)
			}
		})
	}
}

func TestEditMessageTextResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		body string
		err  string
	}{
		{name: "success", body: `{"ok":true,"result":{"message_id":42}}`},
		{name: "malformed JSON", body: `{`, err: "failed to unmarshal telegram send message response"},
		{name: "API error", body: `{"ok":false,"error_code":403,"description":"forbidden"}`, err: "telegram API error 403: forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(test.body)}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
			})}
			err := editMessageText(context.Background(), client, "test-token", "chat", "text", 42)
			assert.True(t, body.closed)
			if test.err == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.err)
			}
		})
	}
}

func TestMessageRequestErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		send func(context.Context, *http.Client, string) error
	}{
		{name: "send", send: func(ctx context.Context, client *http.Client, token string) error {
			id, err := sendMessage(ctx, client, token, "chat", "text")
			assert.Zero(t, id)
			return err
		}},
		{name: "edit", send: func(ctx context.Context, client *http.Client, token string) error {
			return editMessageText(ctx, client, token, "chat", "text", 42)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, cause
			})}
			err := test.send(context.Background(), client, "secret-token")
			require.Error(t, err)
			assert.ErrorIs(t, err, cause)
			var netErr *net.OpError
			assert.ErrorAs(t, err, &netErr)
			assert.NotContains(t, err.Error(), "secret-token")
			assert.NotContains(t, err.Error(), "https://")
			err = test.send(context.Background(), client, "secret-token\n")
			require.ErrorContains(t, err, "failed to build")
			assert.NotContains(t, err.Error(), "secret-token")
		})
	}
}

func TestUnwrapURLError(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection failed")
	assert.Same(t, cause, unwrapURLError(cause))
	assert.NoError(t, unwrapURLError(nil))
	wrapped := fmt.Errorf("request: %w", &url.Error{Op: "Post", URL: "https://example.com/secret-token", Err: cause})
	err := unwrapURLError(wrapped)
	assert.ErrorIs(t, err, cause)
	assert.NotContains(t, err.Error(), "secret-token")
	for _, malformed := range []*url.Error{nil, {}} {
		require.EqualError(t, unwrapURLError(malformed), "telegram request failed without an error cause")
	}
}
