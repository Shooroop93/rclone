package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/atexit"
	"github.com/rclone/rclone/lib/exitcode"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type notificationRunResult struct {
	Attempts int
	Errors   int64
	Messages []string
}

func TestRunNotificationHelper(t *testing.T) {
	scenario := os.Getenv("RCLONE_NOTIFICATION_RUN_TEST")
	if scenario == "" {
		return
	}
	require.NoError(t, config.SetConfigPath(os.Getenv("RCLONE_CONFIG")))
	configfile.Install()
	ci := fs.GetConfig(context.Background())
	ci.Retries = 2
	ci.RetriesInterval = 0
	ci.Progress = false
	ci.Dump = 0
	ci.ErrorOnNoTransfer = scenario == "no-transfer"
	fs.CountError = func(ctx context.Context, err error) error {
		return accounting.Stats(ctx).Error(err)
	}
	accounting.GlobalStats().ResetCounters()
	var mu sync.Mutex
	var result notificationRunResult
	started := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		result.Messages = append(result.Messages, body.Text)
		mu.Unlock()
		select {
		case started <- struct{}{}:
		default:
		}
		if strings.HasPrefix(scenario, "delivery-") {
			http.Error(w, "notification delivery failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	}))
	defer server.Close()
	dialer := &tls.Dialer{Config: server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "api.telegram.org:443" {
			return nil, fmt.Errorf("unexpected notification address %q", address)
		}
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	*notifyProfiles = []string{"personal"}
	if strings.HasPrefix(scenario, "without-") {
		*notifyProfiles = nil
	}
	report := func() {
		mu.Lock()
		defer mu.Unlock()
		result.Errors = accounting.GlobalStats().GetErrors()
		encoded, err := json.Marshal(result)
		if err != nil {
			panic(err)
		}
		fmt.Printf("NOTIFICATION_RESULT=%s\n", encoded)
	}
	// Normal exit reports after Run finishes delivery; cancellation reports after the exit handlers finish.
	if scenario != "cancel" {
		atexit.Register(report)
	}
	command := &cobra.Command{Use: "copy", Annotations: map[string]string{"notification": "true"}}
	require.NoError(t, command.ParseFlags([]string{`C:\source folder`, "remote:destination folder"}))
	Run(scenario == "retry", false, command, func() error {
		result.Attempts++
		if result.Attempts == 1 && len(*notifyProfiles) != 0 {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				panic("notification manager did not start before the operation")
			}
		}
		accounting.GlobalStats().BytesNoNetwork(100)
		switch scenario {
		case "retry":
			if result.Attempts == 1 {
				return fserrors.RetryError(errors.New("first attempt failed"))
			}
		case "operation", "delivery-failure", "without-failure":
			return fs.ErrorDirNotFound
		case "accounting":
			_ = fs.CountError(context.Background(), fs.ErrorDirNotFound)
		case "cancel":
			atexit.Run()
			report()
			return context.Canceled
		}
		return nil
	})
	t.Fatal("Run returned without exiting")
}

func TestRunNotifications(t *testing.T) {
	for _, test := range []struct {
		name     string
		exitCode int
		attempts int
		errors   int64
		messages int
		final    string
	}{
		{"retry", exitcode.Success, 2, 0, 2, "completed successfully"},
		{"operation", exitcode.DirNotFound, 1, 1, 2, fs.ErrorDirNotFound.Error()},
		{"accounting", exitcode.DirNotFound, 1, 1, 2, fs.ErrorDirNotFound.Error()},
		{"delivery-success", exitcode.Success, 1, 0, 4, "completed successfully"},
		{"delivery-failure", exitcode.DirNotFound, 1, 1, 4, fs.ErrorDirNotFound.Error()},
		{"without-success", exitcode.Success, 1, 0, 0, ""},
		{"without-failure", exitcode.DirNotFound, 1, 1, 0, ""},
		{"cancel", exitcode.UncategorizedError, 1, 0, 2, context.Canceled.Error()},
		{"no-transfer", exitcode.NoFilesTransferred, 1, 0, 2, "completed successfully"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "rclone.conf")
			require.NoError(t, os.WriteFile(configPath, []byte("[notify:]\ninterval=1h\nrequest_timeout=1s\nshutdown_timeout=10s\n\n[notify:personal]\nprovider=telegram\ntoken=123:secret\nchat_id=-12345\n"), 0o600))
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			process := exec.CommandContext(ctx, executable, "-test.run=^TestRunNotificationHelper$")
			for _, value := range os.Environ() {
				name, _, _ := strings.Cut(value, "=")
				name = strings.ToUpper(name)
				if strings.HasPrefix(name, "RCLONE_") || name == "HTTP_PROXY" || name == "HTTPS_PROXY" || name == "ALL_PROXY" || name == "NO_PROXY" {
					continue
				}
				process.Env = append(process.Env, value)
			}
			process.Env = append(process.Env, "RCLONE_NOTIFICATION_RUN_TEST="+test.name, "RCLONE_CONFIG="+configPath, "NO_PROXY=*")
			output, err := process.CombinedOutput()
			if test.exitCode == 0 {
				require.NoError(t, err, "%s", output)
			} else {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, "%s", output)
				assert.Equal(t, test.exitCode, exitErr.ExitCode(), "%s", output)
			}
			var result notificationRunResult
			found := false
			for _, line := range strings.Split(string(output), "\n") {
				if encoded, ok := strings.CutPrefix(line, "NOTIFICATION_RESULT="); ok {
					require.False(t, found, "Run must report its result once")
					require.NoError(t, json.Unmarshal([]byte(encoded), &result))
					found = true
				}
			}
			require.True(t, found, "missing Run result: %s", output)
			assert.Equal(t, test.attempts, result.Attempts)
			assert.Equal(t, test.errors, result.Errors)
			require.Len(t, result.Messages, test.messages)
			if test.messages == 0 {
				return
			}
			assert.Contains(t, result.Messages[0], "copy — started")
			for _, message := range result.Messages[1:] {
				assert.Contains(t, message, test.final)
				assert.NotContains(t, message, "first attempt failed")
			}
			for _, message := range result.Messages {
				assert.Contains(t, message, `Source: C:\source folder`)
				assert.Contains(t, message, "Destination: remote:destination folder")
			}
		})
	}
}
