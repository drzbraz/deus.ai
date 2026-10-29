package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: SIGTERM must let in-flight requests finish instead of cutting
// them off mid-write.
func TestServe_DrainsInFlightRequestsOnShutdown(t *testing.T) {
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serve(ctx, srv, ln, 5*time.Second) }()

	responseBody := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			responseBody <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		responseBody <- string(body)
	}()

	<-started
	cancel() // shutdown signal arrives while the request is running

	assert.Equal(t, "done", <-responseBody)
	assert.NoError(t, <-serveErr)

	_, err = net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
	assert.Error(t, err, "no new connections after shutdown")
}

func TestServe_GivesUpAfterShutdownTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-block
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serve(ctx, srv, ln, 50*time.Millisecond) }()
	go func() { _, _ = http.Get("http://" + ln.Addr().String()) }()

	<-started
	cancel()

	assert.ErrorContains(t, <-serveErr, "drain http server")
}

func TestServe_ReportsListenerFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	err = serve(context.Background(), &http.Server{}, ln, time.Second)

	assert.ErrorContains(t, err, "http server")
}

func TestEvery_RunsImmediatelyAndStopsOnCancel(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		every(ctx, time.Millisecond, func(context.Context) { calls.Add(1) })
		close(done)
	}()

	assert.Eventually(t, func() bool { return calls.Load() >= 3 }, time.Second, time.Millisecond)
	cancel()
	<-done
}
