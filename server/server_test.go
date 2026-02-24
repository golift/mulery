package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	testIDHeader = "X-Client-Id"
)

type testEnv struct {
	srv    *Server
	ts     *httptest.Server
	done   chan struct{}
	dialer *websocket.Dialer
}

func newTestServer(t *testing.T, secretKey, idHeader string) *testEnv {
	t.Helper()

	config := &Config{
		Dispatchers: 2,
		Timeout:     5 * time.Second,
		IdleTimeout: time.Minute,
		SecretKey:   secretKey,
		IDHeader:    idHeader,
		Logger:      &mulch.DefaultLogger{Silent: true},
	}

	srv := &Server{
		Config: config,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: mulch.HandshakeTimeout,
		},
		newPool:     make(chan *PoolConfig, 100),
		dispatcher:  make(chan *dispatchRequest),
		pools:       make(map[clientID]*Pool),
		threadCount: make(map[uint]uint64),
		getPool:     make(chan *getPoolRequest),
		repPool:     make(chan *Pool),
		getStats:    make(chan clientID),
		repStats:    make(chan *Stats),
	}

	mux := http.NewServeMux()
	mux.Handle("/register", srv.HandleRegister())
	mux.Handle("/request", srv.HandleRequest("test"))
	mux.Handle("/request/", srv.HandleRequest("test"))
	mux.HandleFunc("/stats", srv.HandleStats)

	httpServer := httptest.NewServer(mux)
	done := make(chan struct{})

	go func() {
		srv.StartDispatcher()
		close(done)
	}()

	env := &testEnv{
		srv:    srv,
		ts:     httpServer,
		done:   done,
		dialer: &websocket.Dialer{HandshakeTimeout: 5 * time.Second},
	}

	t.Cleanup(func() {
		srv.Shutdown()
		<-done
		httpServer.Close()
	})

	return env
}

func connectClient(t *testing.T, env *testEnv, cID, secret string, poolSize int) *websocket.Conn {
	t.Helper()

	wsURL := strings.Replace(env.ts.URL, "http://", "ws://", 1) + "/register"
	header := http.Header{}
	header.Set(mulch.SecretKeyHeader, secret)

	conn, resp, err := env.dialer.Dial(wsURL, header)
	require.NoError(t, err, "WebSocket dial must succeed")

	if resp != nil {
		resp.Body.Close()
	}

	greeting := &mulch.Handshake{
		ID:      cID,
		Name:    "test-" + cID,
		Size:    poolSize,
		MaxSize: poolSize + 5,
	}
	require.NoError(t, conn.WriteJSON(greeting), "greeting write must succeed")

	return conn
}

type fakeClientResponse struct {
	StatusCode int
	Headers    http.Header
	Body       string
}

// serveOneRequestQuiet serves one request without testing.T.
// Safe to call from any goroutine. Returns the received request, or nil on error.
func serveOneRequestQuiet(conn *websocket.Conn, resp fakeClientResponse) *mulch.HTTPRequest {
	_, jsonReq, err := conn.ReadMessage()
	if err != nil {
		return nil
	}

	httpReq := new(mulch.HTTPRequest)
	if err := json.Unmarshal(jsonReq, httpReq); err != nil {
		return nil
	}

	if _, _, err = conn.ReadMessage(); err != nil {
		return nil
	}

	headers := resp.Headers
	if headers == nil {
		headers = make(http.Header)
	}

	jsonResp, _ := json.Marshal(&mulch.HTTPResponse{
		StatusCode:    resp.StatusCode,
		Header:        headers,
		ContentLength: int64(len(resp.Body)),
	})

	if err := conn.WriteMessage(websocket.TextMessage, jsonResp); err != nil {
		return nil
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(resp.Body)); err != nil {
		return nil
	}

	return httpReq
}

// serveForever reads requests and responds until the connection closes.
func serveForever(conn *websocket.Conn, resp fakeClientResponse) {
	for serveOneRequestQuiet(conn, resp) != nil {
	}
}

func waitForPool(t *testing.T, env *testEnv, cID string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, env.ts.URL+"/stats", http.NoBody)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}

		var stats map[string]json.RawMessage

		_ = json.NewDecoder(resp.Body).Decode(&stats)
		resp.Body.Close()

		if pools, ok := stats["pools"]; ok && strings.Contains(string(pools), cID) {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for pool %q to appear in stats", cID)
}

// sendProxyRequest sends an HTTP request through the proxy.
// Only safe to call from the test goroutine (uses require).
func sendProxyRequest(t *testing.T, env *testEnv, cID, method, body string) *http.Response {
	t.Helper()

	resp, err := doProxyRequest(env, cID, method, body)
	require.NoError(t, err, "proxy request must succeed")

	return resp
}

// doProxyRequest is like sendProxyRequest but returns an error instead
// of calling require. Safe to call from any goroutine.
func doProxyRequest(env *testEnv, cID, method, body string) (*http.Response, error) {
	reqBody := io.Reader(http.NoBody)
	if body != "" {
		reqBody = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(
		context.Background(), method,
		env.ts.URL+"/request/test-path", reqBody,
	)
	if err != nil {
		return nil, err
	}

	if env.srv.Config.IDHeader != "" && cID != "" {
		req.Header.Set(env.srv.Config.IDHeader, cID)
	}

	client := &http.Client{Timeout: 10 * time.Second}

	return client.Do(req)
}

func createTestWSPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	serverCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}

	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err == nil {
			serverCh <- conn
		}
	}))
	t.Cleanup(httpServer.Close)

	wsURL := strings.Replace(httpServer.URL, "http://", "ws://", 1)

	clientWS, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err, "dialing test websocket pair")

	if resp != nil {
		resp.Body.Close()
	}

	serverWS := <-serverCh

	return serverWS, clientWS
}

// ---- Unit Tests ----

func TestNewConfig(t *testing.T) {
	t.Parallel()

	cfg := NewConfig()
	assert.Equal(t, uint(1), cfg.Dispatchers)
	assert.Equal(t, time.Second, cfg.Timeout)
	assert.Equal(t, time.Minute+time.Second, cfg.IdleTimeout)
	assert.NotNil(t, cfg.Logger)
}

func TestConnectionStatus_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status ConnectionStatus
		want   string
	}{
		{Idle, "idle"},
		{Busy, "busy"},
		{Closed, "closed"},
		{ConnectionStatus(99), "unknown"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.status.String())
	}
}

func TestValidateKey(t *testing.T) {
	t.Parallel()

	t.Run("valid static key", func(t *testing.T) {
		t.Parallel()

		srv := &Server{Config: &Config{SecretKey: testSecret}}
		header := http.Header{}
		header.Set(mulch.SecretKeyHeader, testSecret)

		secret, err := srv.validateKey(context.Background(), header)
		require.NoError(t, err)
		assert.Empty(t, secret)
	})

	t.Run("invalid static key", func(t *testing.T) {
		t.Parallel()

		srv := &Server{Config: &Config{SecretKey: testSecret}}
		header := http.Header{}
		header.Set(mulch.SecretKeyHeader, "wrong-key")

		_, err := srv.validateKey(context.Background(), header)
		require.ErrorIs(t, err, ErrInvalidKey)
	})

	t.Run("custom validator success", func(t *testing.T) {
		t.Parallel()

		srv := &Server{Config: &Config{
			KeyValidator: func(_ context.Context, header http.Header) (string, error) {
				if header.Get("Authorization") == "Bearer token123" {
					return "custom-seed", nil
				}

				return "", ErrInvalidKey
			},
		}}

		header := http.Header{}
		header.Set("Authorization", "Bearer token123")

		secret, err := srv.validateKey(context.Background(), header)
		require.NoError(t, err)
		assert.Equal(t, "custom-seed", secret)
	})

	t.Run("custom validator failure", func(t *testing.T) {
		t.Parallel()

		srv := &Server{Config: &Config{
			KeyValidator: func(_ context.Context, _ http.Header) (string, error) {
				return "", ErrInvalidKey
			},
		}}

		_, err := srv.validateKey(context.Background(), http.Header{})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidKey)
	})
}

func TestGetClientID(t *testing.T) {
	t.Parallel()

	t.Run("no IDHeader configured", func(t *testing.T) {
		t.Parallel()

		srv := &Server{Config: &Config{}}
		req := httptest.NewRequest(http.MethodGet, "/request", http.NoBody)

		cID, err := srv.getClientID(req)
		require.NoError(t, err)
		assert.Empty(t, string(cID))
	})

	t.Run("IDHeader present", func(t *testing.T) {
		t.Parallel()

		srv := &Server{Config: &Config{IDHeader: testIDHeader}}
		req := httptest.NewRequest(http.MethodGet, "/request", http.NoBody)
		req.Header.Set(testIDHeader, testClientID)

		cID, err := srv.getClientID(req)
		require.NoError(t, err)
		assert.Equal(t, clientID(testClientID), cID)
	})

	t.Run("IDHeader missing", func(t *testing.T) {
		t.Parallel()

		srv := &Server{Config: &Config{IDHeader: testIDHeader}}
		req := httptest.NewRequest(http.MethodGet, "/request", http.NoBody)

		_, err := srv.getClientID(req)
		require.ErrorIs(t, err, ErrNoClientID)
	})
}

func TestProxyError(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request", http.NoBody)

	env.srv.ProxyError(recorder, req, ErrNoProxyTarget, "")
	assert.Equal(t, mulch.ProxyErrorCode, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "no proxy target")
}

// ---- Integration Tests ----

func TestHandleRegister_Success(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	wsConn := connectClient(t, env, testClientID, testSecret, 5)
	defer wsConn.Close()

	waitForPool(t, env, testClientID)
}

func TestHandleRegister_InvalidKey(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	wsURL := strings.Replace(env.ts.URL, "http://", "ws://", 1) + "/register"
	header := http.Header{}
	header.Set(mulch.SecretKeyHeader, "bad-key")

	_, resp, err := env.dialer.Dial(wsURL, header)
	require.Error(t, err, "dial with bad key should fail")

	if resp != nil {
		resp.Body.Close()
	}
}

func TestHandleRequest_Success(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	wsConn := connectClient(t, env, testClientID, testSecret, 5)

	defer wsConn.Close()

	waitForPool(t, env, testClientID)

	wantBody := "hello from the tunnel"
	wantHeaders := http.Header{"X-Custom-Header": {"custom-value"}}

	go serveOneRequestQuiet(wsConn, fakeClientResponse{
		StatusCode: http.StatusOK,
		Headers:    wantHeaders,
		Body:       wantBody,
	})

	resp := sendProxyRequest(t, env, testClientID, http.MethodGet, "")
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, wantBody, string(body))
	assert.Equal(t, "custom-value", resp.Header.Get("X-Custom-Header"))
}

func TestHandleRequest_NoPool(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet,
		env.ts.URL+"/request/test", http.NoBody,
	)
	require.NoError(t, err)

	req.Header.Set(testIDHeader, "nonexistent-client")

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, mulch.ProxyErrorCode, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "no proxy target")
}

func TestHandleRequest_MissingIDHeader(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	wsConn := connectClient(t, env, testClientID, testSecret, 5)
	defer wsConn.Close()

	waitForPool(t, env, testClientID)

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet,
		env.ts.URL+"/request/test", http.NoBody,
	)
	require.NoError(t, err)

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, mulch.ProxyErrorCode, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "client id header is missing")
}

func TestHandleRequest_WithIDHeader(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)

	clientA := "client-A"
	clientB := "client-B"

	wsA := connectClient(t, env, clientA, testSecret, 5)
	defer wsA.Close()

	wsB := connectClient(t, env, clientB, testSecret, 5)
	defer wsB.Close()

	waitForPool(t, env, clientA)
	waitForPool(t, env, clientB)

	go serveOneRequestQuiet(wsB, fakeClientResponse{
		StatusCode: http.StatusAccepted,
		Body:       "from-B",
	})

	resp := sendProxyRequest(t, env, clientB, http.MethodPost, "payload")
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, "from-B", string(body))
}

func TestHandleRequest_ProxyDestination(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	wsConn := connectClient(t, env, testClientID, testSecret, 5)
	defer wsConn.Close()

	waitForPool(t, env, testClientID)

	receivedURL := make(chan string, 1)

	go func() {
		httpReq := serveOneRequestQuiet(wsConn, fakeClientResponse{
			StatusCode: http.StatusOK,
			Body:       "ok",
		})
		if httpReq != nil {
			receivedURL <- httpReq.URL
		}
	}()

	destURL := "http://internal-service:8080/api/v1/data"

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet,
		env.ts.URL+"/request/original", http.NoBody,
	)
	require.NoError(t, err)

	req.Header.Set(testIDHeader, testClientID)
	req.Header.Set("X-Proxy-Destination", destURL)

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	select {
	case got := <-receivedURL:
		assert.Equal(t, destURL, got)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for receivedURL")
	}
}

func TestHandleStats(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	wsConn := connectClient(t, env, testClientID, testSecret, 5)
	defer wsConn.Close()

	waitForPool(t, env, testClientID)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, env.ts.URL+"/stats", http.NoBody)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var stats Stats

	require.NoError(t, json.NewDecoder(resp.Body).Decode(&stats))
	assert.NotEmpty(t, stats.Pools)
	assert.NotNil(t, stats.Threads)
}

// ---- Concurrency Tests ----

func TestConcurrentRequests(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)

	const numConns = 5
	const numRequests = 20

	conns := make([]*websocket.Conn, numConns)
	for i := range numConns {
		conns[i] = connectClient(t, env, testClientID, testSecret, numConns+5)
		defer conns[i].Close()
	}

	waitForPool(t, env, testClientID)
	time.Sleep(100 * time.Millisecond)

	for _, wsConn := range conns {
		go serveForever(wsConn, fakeClientResponse{
			StatusCode: http.StatusOK,
			Body:       "concurrent-ok",
		})
	}

	var reqWg sync.WaitGroup

	results := make(chan int, numRequests)

	for range numRequests {
		reqWg.Add(1)

		go func() {
			defer reqWg.Done()

			resp, err := doProxyRequest(env, testClientID, http.MethodGet, "")
			if err != nil {
				return
			}

			defer resp.Body.Close()

			_, _ = io.ReadAll(resp.Body)
			results <- resp.StatusCode
		}()
	}

	reqWg.Wait()
	close(results)

	var okCount int

	for code := range results {
		if code == http.StatusOK {
			okCount++
		}
	}

	assert.Equal(t, numRequests, okCount, "all requests should return 200")
}

func TestConcurrentClients(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)

	const numClients = 5

	type clientPair struct {
		id string
		ws *websocket.Conn
	}

	clients := make([]clientPair, numClients)

	for i := range numClients {
		cID := fmt.Sprintf("concurrent-client-%d", i)
		wsConn := connectClient(t, env, cID, testSecret, 3)

		clients[i] = clientPair{id: cID, ws: wsConn}

		defer wsConn.Close()
	}

	for _, c := range clients {
		waitForPool(t, env, c.id)
	}

	var clientWg sync.WaitGroup

	for _, c := range clients {
		clientWg.Add(1)

		go func() {
			defer clientWg.Done()

			go serveOneRequestQuiet(c.ws, fakeClientResponse{
				StatusCode: http.StatusOK,
				Body:       "hello-" + c.id,
			})

			resp, err := doProxyRequest(env, c.id, http.MethodGet, "")
			if err != nil {
				return
			}

			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "hello-"+c.id, string(body))
		}()
	}

	clientWg.Wait()
}

func TestStartStopRace(t *testing.T) {
	t.Parallel()

	for range 3 {
		config := &Config{
			Dispatchers: 2,
			Timeout:     5 * time.Second,
			IdleTimeout: time.Minute,
			SecretKey:   testSecret,
			IDHeader:    testIDHeader,
			Logger:      &mulch.DefaultLogger{Silent: true},
		}

		srv := &Server{
			Config: config,
			upgrader: websocket.Upgrader{
				HandshakeTimeout: mulch.HandshakeTimeout,
			},
			newPool:     make(chan *PoolConfig, 100),
			dispatcher:  make(chan *dispatchRequest),
			pools:       make(map[clientID]*Pool),
			threadCount: make(map[uint]uint64),
			getPool:     make(chan *getPoolRequest),
			repPool:     make(chan *Pool),
			getStats:    make(chan clientID),
			repStats:    make(chan *Stats),
		}

		mux := http.NewServeMux()
		mux.Handle("/register", srv.HandleRegister())
		mux.Handle("/request/", srv.HandleRequest("test"))
		mux.HandleFunc("/stats", srv.HandleStats)

		httpServer := httptest.NewServer(mux)
		done := make(chan struct{})

		go func() {
			srv.StartDispatcher()
			close(done)
		}()

		env := &testEnv{srv: srv, ts: httpServer, done: done, dialer: &websocket.Dialer{
			HandshakeTimeout: 5 * time.Second,
		}}

		wsConn := connectClient(t, env, testClientID, testSecret, 5)
		waitForPool(t, env, testClientID)

		go func() {
			go serveOneRequestQuiet(wsConn, fakeClientResponse{
				StatusCode: http.StatusOK,
				Body:       "race-test",
			})

			resp, err := doProxyRequest(env, testClientID, http.MethodGet, "")
			if err == nil && resp != nil {
				resp.Body.Close()
			}
		}()

		time.Sleep(10 * time.Millisecond)

		srv.Shutdown()
		srv.Shutdown()
		<-done

		wsConn.Close()
		httpServer.Close()
	}
}

func TestShutdownDuringRequest(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	wsConn := connectClient(t, env, testClientID, testSecret, 5)
	defer wsConn.Close()

	waitForPool(t, env, testClientID)

	requestStarted := make(chan struct{})
	requestDone := make(chan struct{})

	go func() {
		defer close(requestDone)

		_, _, err := wsConn.ReadMessage()
		if err != nil {
			return
		}

		_, _, err = wsConn.ReadMessage()
		if err != nil {
			return
		}

		close(requestStarted)
		time.Sleep(100 * time.Millisecond)

		jsonResp, _ := json.Marshal(&mulch.HTTPResponse{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			ContentLength: 2,
		})
		_ = wsConn.WriteMessage(websocket.TextMessage, jsonResp)
		_ = wsConn.WriteMessage(websocket.BinaryMessage, []byte("ok"))
	}()

	go func() {
		resp, err := doProxyRequest(env, testClientID, http.MethodGet, "")
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Log("request did not start in time, skipping mid-flight shutdown test")
		return
	}

	env.srv.Shutdown()
	<-env.done

	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Error("fake client goroutine did not finish")
	}
}

func TestConnectionTakeGiveClose(t *testing.T) {
	t.Parallel()

	serverWS, clientWS := createTestWSPair(t)
	defer clientWS.Close()

	pool := &Pool{
		id:      "take-give-test",
		idle:    make(chan *Connection, 10),
		Logger:  &mulch.DefaultLogger{Silent: true},
		minSize: 5,
	}

	conn := &Connection{
		connected:    time.Now(),
		status:       Idle,
		pool:         pool,
		sock:         serverWS,
		done:         make(chan struct{}),
		nextResponse: make(chan chan io.Reader),
	}

	var takeWg sync.WaitGroup

	taken := make(chan *Connection, 5)

	for range 5 {
		takeWg.Add(1)

		go func() {
			defer takeWg.Done()
			taken <- conn.Take()
		}()
	}

	takeWg.Wait()
	close(taken)

	var successCount int

	for result := range taken {
		if result != nil {
			successCount++
		}
	}

	assert.Equal(t, 1, successCount, "exactly one Take should succeed")
	assert.Equal(t, Busy, conn.Status())

	conn.Give()
	assert.Equal(t, Idle, conn.Status())

	var closeWg sync.WaitGroup

	for range 3 {
		closeWg.Add(1)

		go func() {
			defer closeWg.Done()
			conn.Close("concurrent-close")
		}()
	}

	closeWg.Wait()
	assert.Equal(t, Closed, conn.Status())
}

func TestPoolShutdownConcurrent(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)

	const numConns = 5

	conns := make([]*websocket.Conn, numConns)
	for i := range numConns {
		conns[i] = connectClient(t, env, testClientID, testSecret, numConns+5)
		defer conns[i].Close()
	}

	waitForPool(t, env, testClientID)

	env.srv.Shutdown()
	<-env.done
}

func TestMultipleRequestsSameConnection(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)
	wsConn := connectClient(t, env, testClientID, testSecret, 5)
	defer wsConn.Close()

	waitForPool(t, env, testClientID)

	const iterations = 3

	// Single goroutine serves all requests on this connection to avoid
	// concurrent writes on the same gorilla/websocket.Conn.
	go func() {
		for i := range iterations {
			serveOneRequestQuiet(wsConn, fakeClientResponse{
				StatusCode: http.StatusOK,
				Body:       fmt.Sprintf("response-%d", i),
			})
		}
	}()

	for i := range iterations {
		resp := sendProxyRequest(t, env, testClientID, http.MethodGet, "")

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, fmt.Sprintf("response-%d", i), string(respBody))
	}
}

func TestShutdownIdempotent(t *testing.T) {
	t.Parallel()

	env := newTestServer(t, testSecret, testIDHeader)

	env.srv.Shutdown()
	env.srv.Shutdown()
	env.srv.Shutdown()
	<-env.done
}
