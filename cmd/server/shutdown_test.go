package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

// A request that arrived before SIGTERM must finish, not be dropped.
//
// This is what a rolling deploy does to every instance, several times, and the
// failure it prevents is invisible on a healthy platform: users who did
// nothing but arrive at the wrong moment get an error, which reads as an
// intermittent platform fault rather than as a deployment. That is what makes
// it expensive — the deploy is the last place anybody looks.
//
// The handler holds the request open past the signal, which is the only
// arrangement that distinguishes draining from stopping. A handler that
// returns immediately would pass against a server that closed its listener and
// exited on the spot.
func TestARequestInFlightSurvivesShutdown(t *testing.T) {
	arrived := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		// Longer than the gap between the signal and the assertion below, so
		// the request is genuinely still running when shutdown begins.
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("finished"))
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	returned := make(chan error, 1)
	go func() { returned <- serveUntilSignal(server, serverErrors, signals, 5*time.Second) }()

	type outcome struct {
		status int
		body   string
		err    error
	}
	response := make(chan outcome, 1)
	go func() {
		result, err := http.Get(fmt.Sprintf("http://%s/slow", listener.Addr()))
		if err != nil {
			response <- outcome{err: err}
			return
		}
		defer result.Body.Close()
		body := make([]byte, 8)
		n, _ := result.Body.Read(body)
		response <- outcome{status: result.StatusCode, body: string(body[:n])}
	}()

	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	// The signal lands while the handler is still running.
	signals <- syscall.SIGTERM

	select {
	case got := <-response:
		if got.err != nil {
			t.Fatalf("the request in flight was dropped by shutdown: %v", got.err)
		}
		if got.status != http.StatusOK {
			t.Errorf("status = %d, want 200", got.status)
		}
		if got.body != "finished" {
			t.Errorf("body = %q, want %q", got.body, "finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request never completed")
	}

	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("serveUntilSignal returned %v, want nil after a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilSignal did not return after draining")
	}

	// And the listener is closed, so nothing new is accepted afterwards.
	if _, err := http.Get(fmt.Sprintf("http://%s/slow", listener.Addr())); err == nil {
		t.Error("the server still accepts requests after shutdown returned")
	}
}

// A server that fails for its own reasons must report it rather than be
// mistaken for a clean stop. ErrServerClosed is the one error that means the
// stop was deliberate.
func TestAServerFailureIsReportedAndACloseIsNot(t *testing.T) {
	server := &http.Server{}
	signals := make(chan os.Signal, 1)

	failure := errors.New("listen: address already in use")
	errs := make(chan error, 1)
	errs <- failure
	if got := serveUntilSignal(server, errs, signals, time.Second); !errors.Is(got, failure) {
		t.Errorf("a server failure returned %v, want it reported", got)
	}

	closed := make(chan error, 1)
	closed <- http.ErrServerClosed
	if got := serveUntilSignal(server, closed, signals, time.Second); got != nil {
		t.Errorf("a deliberate close returned %v, want nil", got)
	}
}
