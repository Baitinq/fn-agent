package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func startTestSession(t *testing.T, a *Agent) {
	t.Helper()
	if err := a.StartSession("test", t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func sseServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join(frames, "\n\n")+"\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}
