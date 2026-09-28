package cmdtest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotificationCLIHelper(t *testing.T) {
	if os.Getenv("RCLONE_NOTIFICATION_CLI_HELPER") != "1" {
		return
	}
	certificateDER, err := base64.StdEncoding.DecodeString(os.Getenv("RCLONE_NOTIFICATION_CLI_CERT"))
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(certificateDER)
	require.NoError(t, err)
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(certificate)
	dialer := &tls.Dialer{Config: &tls.Config{RootCAs: rootCAs}}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// fshttp copies this dialer into each new notification client.
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "api.telegram.org:443" {
			return nil, fmt.Errorf("unexpected notification address %q", address)
		}
		return dialer.DialContext(ctx, network, os.Getenv("RCLONE_NOTIFICATION_CLI_SERVER"))
	}
	http.DefaultTransport = transport
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"rclone"}, os.Args[i+1:]...)
			main()
			t.Fatal("rclone returned without exiting")
		}
	}
	t.Fatal("missing rclone arguments")
}

func TestNotificationCLI(t *testing.T) {
	for _, operation := range []string{"copy", "sync", "move"} {
		t.Run(operation, func(t *testing.T) {
			type request struct {
				ChatID    string `json:"chat_id"`
				MessageID int    `json:"message_id"`
				Text      string `json:"text"`
				path      string
			}
			var mu sync.Mutex
			var requests []request
			finalRequested := make(chan struct{}, 1)
			finalRelease := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(finalRelease) }) }
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "api.telegram.org", r.Host)
				var received request
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Errorf("decode notification: %v", err)
					http.Error(w, "invalid notification", http.StatusBadRequest)
					return
				}
				received.path = r.URL.Path
				mu.Lock()
				requests = append(requests, received)
				mu.Unlock()
				if received.path == "/bot123:secret/editMessageText" {
					select {
					case finalRequested <- struct{}{}:
					default:
					}
					<-finalRelease
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(release)

			dir := t.TempDir()
			source := filepath.Join(dir, "source folder")
			destination := filepath.Join(dir, "destination folder")
			require.NoError(t, os.Mkdir(source, 0o700))
			content := []byte("notification CLI integration test\n")
			require.NoError(t, os.WriteFile(filepath.Join(source, "example.txt"), content, 0o600))
			configPath := filepath.Join(dir, "rclone.conf")
			require.NoError(t, os.WriteFile(configPath, []byte("[notify:]\ninterval = 1h\n\n[notify:personal]\nprovider = telegram\ntoken = 123:secret\nchat_id = -12345\n"), 0o600))
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			process := exec.CommandContext(ctx, executable, "-test.run=^TestNotificationCLIHelper$", "--",
				operation, source, destination, "--config", configPath,
				"--notify", "personal", "--stats", "0")
			for _, value := range os.Environ() {
				name, _, _ := strings.Cut(value, "=")
				name = strings.ToUpper(name)
				if strings.HasPrefix(name, "RCLONE_") || name == "HTTP_PROXY" || name == "HTTPS_PROXY" || name == "ALL_PROXY" || name == "NO_PROXY" {
					continue
				}
				process.Env = append(process.Env, value)
			}
			process.Env = append(process.Env,
				"RCLONE_NOTIFICATION_CLI_HELPER=1",
				"RCLONE_NOTIFICATION_CLI_SERVER="+server.Listener.Addr().String(),
				"RCLONE_NOTIFICATION_CLI_CERT="+base64.StdEncoding.EncodeToString(server.Certificate().Raw),
				"NO_PROXY=*",
			)
			type result struct {
				output []byte
				err    error
			}
			finished := make(chan result, 1)
			go func() {
				output, err := process.CombinedOutput()
				finished <- result{output: output, err: err}
			}()
			if operation == "copy" {
				select {
				case <-finalRequested:
				case outcome := <-finished:
					t.Fatalf("rclone exited before final notification: %v\n%s", outcome.err, outcome.output)
				case <-ctx.Done():
					t.Fatal("timed out waiting for final notification")
				}
				select {
				case outcome := <-finished:
					t.Fatalf("rclone exited before final delivery completed: %v\n%s", outcome.err, outcome.output)
				case <-time.After(50 * time.Millisecond):
				}
			}
			release()
			outcome := <-finished
			require.NoError(t, outcome.err, "%s", outcome.output)
			copied, err := os.ReadFile(filepath.Join(destination, "example.txt"))
			require.NoError(t, err)
			assert.Equal(t, content, copied)
			_, err = os.Stat(filepath.Join(source, "example.txt"))
			if operation == "move" {
				assert.ErrorIs(t, err, os.ErrNotExist)
			} else {
				assert.NoError(t, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if operation != "copy" {
				assert.Empty(t, requests, "unsupported commands must not send notifications")
				assert.Contains(t, string(outcome.output), "--notify is not supported by "+operation)
				return
			}
			require.Len(t, requests, 2)
			assert.Equal(t, "/bot123:secret/sendMessage", requests[0].path)
			assert.Zero(t, requests[0].MessageID)
			assert.Contains(t, requests[0].Text, operation+" — started")
			assert.Equal(t, "/bot123:secret/editMessageText", requests[1].path)
			assert.Equal(t, 42, requests[1].MessageID)
			assert.Contains(t, requests[1].Text, operation+" — completed successfully")
			for _, received := range requests {
				assert.Equal(t, "-12345", received.ChatID)
				assert.Contains(t, received.Text, "Source: "+source)
				assert.Contains(t, received.Text, "Destination: "+destination)
			}
		})
	}
}
