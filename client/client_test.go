package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golift.io/mulery/mulch"
)

const (
	testSecret   = "test-secret-key"
	testClientID = "test-client-1"
	testTimeout  = 5 * time.Second
)

// --- Test Helpers ---

// fakeServer simulates a mulery server WebSocket endpoint.
type fakeServer struct {
	ts        *httptest.Server
	upgrader  websocket.Upgrader
	secretKey string
	conns     chan *fakeServerConn
	connCount atomic.Int64
}

// fakeServerConn represents a single client connection on the server side.
type fakeServerConn struct {
	ws       *websocket.Conn
	greeting mulch.Handshake
	mu       sync.Mutex
}

func newFakeServer(t *testing.T, secretKey string) *fakeServer {
	t.Helper()

	fs := &fakeServer{
		secretKey: secretKey,
		conns:     make(chan *fakeServerConn, 100),
	}

	fs.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get(mulch.SecretKeyHeader); key != secretKey {
			http.Error(w, "invalid key", http.StatusForbidden)
			return
		}

		conn, err := fs.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		var greeting mulch.Handshake
		if err := conn.ReadJSON(&greeting); err != nil {
			conn.Close()
			return
		}

		fs.connCount.Add(1)
		fs.conns <- &fakeServerConn{ws: conn, greeting: greeting}
	}))

	t.Cleanup(fs.ts.Close)

	return fs
}

func (fs *fakeServer) wsURL() string {
	return strings.Replace(fs.ts.URL, "http://", "ws://", 1)
}

// sendRequest sends a serialized HTTP request to the client via WebSocket
// and reads back the response headers and body.
func (fc *fakeServerConn) sendRequest(req *mulch.HTTPRequest, body string) (*mulch.HTTPResponse, string, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	jsonReq, err := json.Marshal(req)
	if err != nil {
		return nil, "", fmt.Errorf("marshaling request: %w", err)
	}

	if err := fc.ws.WriteMessage(websocket.TextMessage, jsonReq); err != nil {
		return nil, "", fmt.Errorf("writing request: %w", err)
	}

	bodyWriter, err := fc.ws.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return nil, "", fmt.Errorf("getting body writer: %w", err)
	}

	if _, err := bodyWriter.Write([]byte(body)); err != nil {
		return nil, "", fmt.Errorf("writing body: %w", err)
	}

	if err := bodyWriter.Close(); err != nil {
		return nil, "", fmt.Errorf("closing body writer: %w", err)
	}

	_, jsonResp, err := fc.ws.ReadMessage()
	if err != nil {
		return nil, "", fmt.Errorf("reading response: %w", err)
	}

	var resp mulch.HTTPResponse
	if err := json.Unmarshal(jsonResp, &resp); err != nil {
		return nil, "", fmt.Errorf("unmarshaling response: %w", err)
	}

	_, respBody, err := fc.ws.ReadMessage()
	if err != nil {
		return nil, "", fmt.Errorf("reading response body: %w", err)
	}

	return &resp, string(respBody), nil
}

func (fs *fakeServer) waitForConns(t *testing.T, count int) []*fakeServerConn {
	t.Helper()

	var conns []*fakeServerConn
	deadline := time.After(testTimeout)

	for len(conns) < count {
		select {
		case conn := <-fs.conns:
			conns = append(conns, conn)
		case <-deadline:
			t.Fatalf("timed out waiting for %d connections, got %d", count, len(conns))
		}
	}

	return conns
}

func newUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts
}

func makeTestClient(targets []string, opts ...func(*Config)) *Client {
	cfg := &Config{
		ID:            testClientID,
		Name:          "test-client",
		Targets:       targets,
		PoolIdleSize:  1,
		PoolMaxSize:   5,
		SecretKey:     testSecret,
		CleanInterval: time.Second,
		Logger:        &mulch.DefaultLogger{Silent: true},
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return NewClient(cfg)
}

func waitForPoolIdle(t *testing.T, c *Client, wantIdle int) {
	t.Helper()

	deadline := time.After(testTimeout)

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d idle connections", wantIdle)
		default:
		}

		for _, ps := range c.PoolStats() {
			if ps.Idle >= wantIdle {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// --- Unit Tests ---

func TestNewConfig(t *testing.T) {
	t.Parallel()

	cfg := NewConfig()
	assert.Equal(t, DefaultPoolIdleSize, cfg.PoolIdleSize)
	assert.Equal(t, DefaultPoolMaxSize, cfg.PoolMaxSize)
	assert.Equal(t, DefaultMaxBackoff, cfg.MaxBackoff)
	assert.Equal(t, DefaultBackoffReset, cfg.BackoffReset)
	assert.NotNil(t, cfg.Logger)
	assert.Equal(t, time.Second, cfg.CleanInterval)
	assert.Len(t, cfg.Targets, 1)
	assert.Equal(t, "ws://127.0.0.1:8080/register", cfg.Targets[0])
}

func TestNewClient_Defaults(t *testing.T) {
	t.Parallel()

	cfg := NewConfig()
	c := NewClient(cfg)
	assert.NotNil(t, c.client)
	assert.NotNil(t, c.dialer)
	assert.NotNil(t, c.pools)
	assert.Empty(t, c.pools)
	assert.Equal(t, -1, c.target)
	assert.Equal(t, DefaultMaxBackoff, c.MaxBackoff)
	assert.Equal(t, DefaultBackoffReset, c.BackoffReset)
}

func TestNewClient_NilLogger(t *testing.T) {
	t.Parallel()

	c := NewClient(&Config{Targets: []string{"ws://localhost"}})
	assert.NotNil(t, c.Logger)
}

func TestNewClient_SmallCleanInterval(t *testing.T) {
	t.Parallel()

	c := NewClient(&Config{CleanInterval: time.Millisecond})
	assert.Equal(t, time.Second, c.CleanInterval)
}

func TestNewClient_RoundRobinSingleTarget(t *testing.T) {
	t.Parallel()

	c := NewClient(&Config{
		Targets:          []string{"ws://localhost:1234"},
		RoundRobinConfig: &RoundRobinConfig{RetryInterval: time.Minute},
	})
	assert.Nil(t, c.RoundRobinConfig, "RR should be disabled with a single target")
}

func TestNewClient_RoundRobinMultipleTargets(t *testing.T) {
	t.Parallel()

	c := NewClient(&Config{
		Targets:          []string{"ws://a", "ws://b"},
		RoundRobinConfig: &RoundRobinConfig{},
	})
	require.NotNil(t, c.RoundRobinConfig)
	assert.Equal(t, time.Minute, c.RetryInterval, "default RetryInterval should be 1 minute")
}

func TestGetID(t *testing.T) {
	t.Parallel()

	c := NewClient(&Config{SecretKey: "secret", ID: "myid"})
	assert.Equal(t, mulch.HashKeyID("secret", "myid"), c.GetID())
}

func TestGetID_EmptySecret(t *testing.T) {
	t.Parallel()

	c := NewClient(&Config{ID: "myid"})
	assert.Equal(t, "myid", c.GetID())
}

func TestPoolSize_String(t *testing.T) {
	t.Parallel()

	ps := &PoolSize{Connecting: 1, Idle: 2, Running: 3, Total: 6}
	assert.Equal(t, "Connecting 1, idle 2, running 3, total 6", ps.String())
}

// --- Connection & Handler Tests ---

func TestClientConnect(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 1)
	assert.Equal(t, testClientID, conns[0].greeting.ID)
	assert.Equal(t, "test-client", conns[0].greeting.Name)
}

func TestClientConnect_MultipleTargets(t *testing.T) {
	t.Parallel()

	fs1 := newFakeServer(t, testSecret)
	fs2 := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs1.wsURL(), fs2.wsURL()})

	c.Start(t.Context())
	defer c.Shutdown()

	fs1.waitForConns(t, 1)
	fs2.waitForConns(t, 1)

	stats := c.PoolStats()
	assert.Len(t, stats, 2)
}

func TestClientConnect_InvalidKey(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.SecretKey = "wrong-key"
	})

	c.Start(t.Context())
	defer c.Shutdown()

	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int64(0), fs.connCount.Load(), "no connections should succeed with wrong key")
}

func TestDefaultHandler(t *testing.T) {
	t.Parallel()

	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test", "upstream-value")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "hello from upstream")
	})

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 1)

	resp, body, err := conns[0].sendRequest(&mulch.HTTPRequest{
		Method: http.MethodGet,
		URL:    upstream.URL + "/test-path",
	}, "")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "hello from upstream", body)
	assert.Equal(t, "upstream-value", resp.Header.Get("X-Test"))
}

func TestDefaultHandler_PostWithBody(t *testing.T) {
	t.Parallel()

	upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "received: %s", string(b))
	})

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 1)

	resp, body, err := conns[0].sendRequest(&mulch.HTTPRequest{
		Method: http.MethodPost,
		URL:    upstream.URL + "/create",
	}, "request payload")

	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "received: request payload", body)
}

func TestCustomHandler(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.Handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Custom", "handler-value")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, "custom response for "+r.URL.Path)
		}
	})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 1)

	resp, body, err := conns[0].sendRequest(&mulch.HTTPRequest{
		Method: http.MethodGet,
		URL:    "http://example.com/api/test",
	}, "")

	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, "custom response for /api/test", body)
	assert.Equal(t, "handler-value", resp.Header.Get("X-Custom"))
}

func TestCustomHandler_ImplicitWriteHeader(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.Handler = func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "no explicit status")
		}
	})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 1)

	resp, body, err := conns[0].sendRequest(&mulch.HTTPRequest{
		Method: http.MethodGet,
		URL:    "http://example.com/",
	}, "")

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no explicit status", body)
}

func TestCustomHandler_HeaderAccess(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.Handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Multi", "val1")
			w.Header().Add("X-Multi", "val2")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		}
	})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 1)

	resp, body, err := conns[0].sendRequest(&mulch.HTTPRequest{
		Method: http.MethodGet,
		URL:    "http://example.com/headers",
	}, "")

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"ok":true}`, body)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, []string{"val1", "val2"}, resp.Header.Values("X-Multi"))
}

func TestMultipleRequestsSameConnection(t *testing.T) {
	t.Parallel()

	var reqCount atomic.Int64

	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		n := reqCount.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "response-%d", n)
	})

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 1)

	for i := range 3 {
		resp, body, err := conns[0].sendRequest(&mulch.HTTPRequest{
			Method: http.MethodGet,
			URL:    fmt.Sprintf("%s/req/%d", upstream.URL, i),
		}, "")
		require.NoError(t, err, "request %d", i)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotEmpty(t, body)
	}

	assert.Equal(t, int64(3), reqCount.Load())
}

// --- Pool & Stats Tests ---

func TestPoolStats(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.PoolIdleSize = 3
		cfg.PoolMaxSize = 10
	})

	c.Start(t.Context())
	defer c.Shutdown()

	fs.waitForConns(t, 3)
	waitForPoolIdle(t, c, 3)

	stats := c.PoolStats()
	require.Len(t, stats, 1)

	for _, ps := range stats {
		assert.Equal(t, 3, ps.Total)
		assert.Equal(t, 3, ps.Idle)
		assert.Equal(t, 0, ps.Running)
		assert.Equal(t, 0, ps.Connecting)
		assert.True(t, ps.Active)
	}
}

func TestPoolStats_AfterShutdown(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()})

	c.Start(t.Context())
	fs.waitForConns(t, 1)

	c.Shutdown()

	stats := c.PoolStats()
	require.Len(t, stats, 1)

	for _, ps := range stats {
		assert.False(t, ps.Active)
	}
}

func TestPoolStats_Disconnects(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.PoolIdleSize = 2
		cfg.PoolMaxSize = 10
	})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 2)
	waitForPoolIdle(t, c, 2)

	// Close one server-side connection to cause a disconnect.
	conns[0].ws.Close()

	// Wait for client to detect disconnect and reconnect.
	time.Sleep(300 * time.Millisecond)

	stats := c.PoolStats()
	for _, ps := range stats {
		assert.Positive(t, ps.Disconnects)
	}
}

// --- Concurrency & Race Tests ---

func TestConcurrentConnections(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.PoolIdleSize = 5
		cfg.PoolMaxSize = 10
	})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 5)
	assert.Len(t, conns, 5)

	for _, conn := range conns {
		assert.Equal(t, testClientID, conn.greeting.ID)
	}

	waitForPoolIdle(t, c, 5)
}

func TestConcurrentRequests(t *testing.T) {
	t.Parallel()

	var reqCount atomic.Int64

	upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "response for %s", r.URL.Path)
	})

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.PoolIdleSize = 5
		cfg.PoolMaxSize = 10
	})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 5)
	waitForPoolIdle(t, c, 5)

	var wg sync.WaitGroup

	results := make(chan *mulch.HTTPResponse, len(conns))

	for i, conn := range conns {
		wg.Add(1)

		go func(idx int, fc *fakeServerConn) {
			defer wg.Done()

			resp, _, err := fc.sendRequest(&mulch.HTTPRequest{
				Method: http.MethodGet,
				URL:    fmt.Sprintf("%s/path/%d", upstream.URL, idx),
			}, "")
			if err == nil {
				results <- resp
			}
		}(i, conn)
	}

	wg.Wait()
	close(results)

	var okCount int
	for resp := range results {
		if resp.StatusCode == http.StatusOK {
			okCount++
		}
	}

	assert.Equal(t, len(conns), okCount, "all concurrent requests should succeed")
	assert.Equal(t, int64(len(conns)), reqCount.Load())
}

func TestConcurrentRequests_CustomHandler(t *testing.T) {
	t.Parallel()

	var handleCount atomic.Int64

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.PoolIdleSize = 5
		cfg.PoolMaxSize = 10
		cfg.Handler = func(w http.ResponseWriter, r *http.Request) {
			handleCount.Add(1)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "handled %s", r.URL.Path)
		}
	})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 5)
	waitForPoolIdle(t, c, 5)

	var wg sync.WaitGroup

	results := make(chan *mulch.HTTPResponse, len(conns))

	for i, conn := range conns {
		wg.Add(1)

		go func(idx int, fc *fakeServerConn) {
			defer wg.Done()

			resp, _, err := fc.sendRequest(&mulch.HTTPRequest{
				Method: http.MethodGet,
				URL:    fmt.Sprintf("http://example.com/api/%d", idx),
			}, "")
			if err == nil {
				results <- resp
			}
		}(i, conn)
	}

	wg.Wait()
	close(results)

	var okCount int
	for resp := range results {
		if resp.StatusCode == http.StatusOK {
			okCount++
		}
	}

	assert.Equal(t, len(conns), okCount)
	assert.Equal(t, int64(len(conns)), handleCount.Load())
}

func TestStartStopRace(t *testing.T) {
	t.Parallel()

	for range 3 {
		fs := newFakeServer(t, testSecret)
		c := makeTestClient([]string{fs.wsURL()})

		ctx, cancel := context.WithCancel(context.Background())

		c.Start(ctx)
		fs.waitForConns(t, 1)

		var wg sync.WaitGroup

		for range 5 {
			wg.Go(func() {
				c.Shutdown()
			})
		}

		wg.Wait()
		cancel()
	}
}

func TestPoolShutdown_Idempotent(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()})

	c.Start(t.Context())
	fs.waitForConns(t, 1)

	c.Shutdown()
	c.Shutdown()
	c.Shutdown()
}

func TestPoolShutdown_ConcurrentWithRemove(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.PoolIdleSize = 3
		cfg.PoolMaxSize = 5
	})

	c.Start(t.Context())
	serverConns := fs.waitForConns(t, 3)

	var wg sync.WaitGroup

	// Close server-side connections to trigger Remove() calls concurrently with shutdown.
	wg.Go(func() {
		for _, sc := range serverConns {
			sc.ws.Close()
		}
	})

	wg.Go(func() {
		c.Shutdown()
	})

	wg.Wait()
}

func TestShutdownDuringRequest(t *testing.T) {
	t.Parallel()

	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "delayed")
	})

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()})

	c.Start(t.Context())

	conns := fs.waitForConns(t, 1)

	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _, _ = conns[0].sendRequest(&mulch.HTTPRequest{
			Method: http.MethodGet,
			URL:    upstream.URL + "/slow",
		}, "")
	}()

	time.Sleep(50 * time.Millisecond)
	c.Shutdown()

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("request goroutine did not complete after shutdown")
	}
}

func TestConnectionClose_AfterServeExit(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)
	c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
		cfg.PoolIdleSize = 3
		cfg.PoolMaxSize = 5
	})

	c.Start(t.Context())

	serverConns := fs.waitForConns(t, 3)

	// Close all server-side connections, causing serve() to exit.
	for _, sc := range serverConns {
		sc.ws.Close()
	}

	// Wait for serve() goroutines to process the disconnections.
	time.Sleep(300 * time.Millisecond)

	// Shutdown calls Close() on connections whose serve() already exited.
	// This must not panic (validates the signalShutdown fix).
	c.Shutdown()
}

func TestStartStopSequential(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)

	for i := range 3 {
		c := makeTestClient([]string{fs.wsURL()}, func(cfg *Config) {
			cfg.ID = fmt.Sprintf("client-%d", i)
		})

		ctx, cancel := context.WithCancel(context.Background())

		c.Start(ctx)
		fs.waitForConns(t, 1)
		c.Shutdown()

		cancel()
	}
}

// --- Round-Robin Tests ---

func TestRoundRobin_Callback(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)

	var mu sync.Mutex

	var targets []string

	c := makeTestClient([]string{fs.wsURL(), "ws://unreachable:9999"}, func(cfg *Config) {
		cfg.RoundRobinConfig = &RoundRobinConfig{
			RetryInterval: time.Minute,
			Callback: func(_ context.Context, socket string) {
				mu.Lock()
				targets = append(targets, socket)
				mu.Unlock()
			},
		}
	})

	c.Start(t.Context())
	defer c.Shutdown()

	fs.waitForConns(t, 1)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, targets, 1)
	assert.Equal(t, fs.wsURL(), targets[0])
}

func TestRoundRobin_SwitchesTarget(t *testing.T) {
	t.Parallel()

	fs1 := newFakeServer(t, testSecret)
	fs2 := newFakeServer(t, testSecret)

	var mu sync.Mutex

	var callbackTargets []string

	c := makeTestClient([]string{fs1.wsURL(), fs2.wsURL()}, func(cfg *Config) {
		cfg.PoolIdleSize = 1
		cfg.PoolMaxSize = 3
		cfg.RoundRobinConfig = &RoundRobinConfig{
			RetryInterval: 200 * time.Millisecond,
			Callback: func(_ context.Context, socket string) {
				mu.Lock()
				callbackTargets = append(callbackTargets, socket)
				mu.Unlock()
			},
		}
	})

	c.Start(t.Context())
	defer c.Shutdown()

	// Wait for connection to first target.
	fs1.waitForConns(t, 1)

	// Close server1 entirely to force failover.
	fs1.ts.Close()

	// Wait for client to switch to second target.
	fs2.waitForConns(t, 1)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(callbackTargets), 2)
	assert.Equal(t, fs1.wsURL(), callbackTargets[0])
	assert.Equal(t, fs2.wsURL(), callbackTargets[1])
}

func TestRoundRobin_PoolIdleSize(t *testing.T) {
	t.Parallel()

	fs := newFakeServer(t, testSecret)

	c := makeTestClient([]string{fs.wsURL(), "ws://unreachable:9999"}, func(cfg *Config) {
		cfg.PoolIdleSize = 2
		cfg.PoolMaxSize = 5
		cfg.RoundRobinConfig = &RoundRobinConfig{
			RetryInterval: time.Minute,
		}
	})

	c.Start(t.Context())
	defer c.Shutdown()

	conns := fs.waitForConns(t, 2)
	for _, conn := range conns {
		assert.Equal(t, 2, conn.greeting.Size)
	}
}

// --- Middleware (req2Handler) Tests ---

func TestReq2Handler_InterfaceCompliance(t *testing.T) {
	t.Parallel()

	var _ http.ResponseWriter = &req2Handler{}
}
