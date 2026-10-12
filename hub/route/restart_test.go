package route

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/metacubex/mihomo/hub/executor"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
)

func TestRestartAcknowledgmentCompletesBeforeHandlerReturns(t *testing.T) {
	release, callbackEntered, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		writeRestartAcknowledgment(w)
		close(callbackEntered)
		// A successful exec never returns. Keep the handler alive so the HTTP
		// server cannot finish a chunked response on our behalf in this test.
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Post(server.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("restart acknowledgment requires handler completion: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "{\"status\":\"ok\"}\n" {
		t.Fatalf("invalid restart acknowledgment: %d %q", response.StatusCode, body)
	}
	if response.ContentLength != int64(len(body)) || len(response.TransferEncoding) != 0 {
		t.Fatalf("restart acknowledgment depends on chunk termination: length %d, encoding %v", response.ContentLength, response.TransferEncoding)
	}
	select {
	case <-callbackEntered:
	case <-time.After(time.Second):
		t.Fatal("restart callback did not start after flushing the acknowledgment")
	}
	select {
	case <-handlerDone:
		t.Fatal("test handler returned before verifying the complete response")
	default:
	}
}

func TestRestartProcessDoesNotExecAfterStopRequest(t *testing.T) {
	const childFlag = "MIHOMO_TEST_RESTART_AFTER_STOP"
	if os.Getenv(childFlag) == "1" {
		executor.RequestShutdown()
		// Reaching the OS process replacement call would fail and exit through
		// restartProcess's fatal error path. A requested stop must just return.
		restartProcess(filepath.Join(t.TempDir(), "must-not-exec"))
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestRestartProcessDoesNotExecAfterStopRequest$")
	command.Env = append(os.Environ(), childFlag+"=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restart ignored the stop request: %v\n%s", err, output)
	}
}
