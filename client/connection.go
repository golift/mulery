package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golift.io/mulery/mulch"
)

// Status of a Connection.
const (
	UNKNOWN = iota - 1
	SHUTDOWN
	CONNECTING
	IDLE
	RUNNING
)

const (
	keepAliveInterval = 55 * time.Second
	keepAliveTimeout  = 5 * time.Second
)

// Connection handle a single websocket (HTTP/TCP) connection to an Server.
type Connection struct {
	pool      *Pool
	ws        *websocket.Conn
	status    int
	setStatus chan int
	getStatus chan int
	wg        sync.WaitGroup
	id        string
}

// NewConnection creates a Connection object.
func NewConnection(pool *Pool) *Connection {
	return &Connection{
		pool:      pool,
		status:    CONNECTING,
		setStatus: make(chan int),
		getStatus: make(chan int),
		wg:        sync.WaitGroup{},
		id:        strconv.Itoa(rand.Intn(899) + 100), //nolint:mnd,gosec
	}
}

// Connect to the remote server using an HTTP websocket.
func (c *Connection) Connect(ctx context.Context) error {
	c.pool.client.Printf("[%s] Connecting to tunnel @ %s", c.id, c.pool.target)

	var err error
	// Create a new TCP(/TLS) connection (no use of net.http).
	//nolint:bodyclose // Gets closed in the Close() method.
	c.ws, _, err = c.pool.client.dialer.DialContext(
		ctx,
		c.pool.target,
		http.Header{mulch.SecretKeyHeader: {c.pool.secretKey}},
	)
	if err != nil {
		return fmt.Errorf("[%s] tcp dialer failure: %w", c.id, err)
	}

	// c.ws.EnableWriteCompression(true)
	// _ = c.ws.SetCompressionLevel(flate.BestCompression)

	// Send the greeting message with proxy id and desired pool size.
	greeting := &mulch.Handshake{
		Name:      c.pool.client.Name,
		ID:        c.pool.client.ID,
		Size:      c.pool.client.PoolIdleSize,
		MaxSize:   c.pool.client.PoolMaxSize,
		ClientIDs: c.pool.client.ClientIDs,
	}

	if err := c.ws.WriteJSON(greeting); err != nil {
		c.pool.Remove(c)
		return fmt.Errorf("[%s] greeting failure: %w", c.id, err)
	}

	// We are connected to the server, now start a go routine that waits for incoming server requests.
	c.wg.Add(1)
	go c.serve()
	go c.keepAlive()

	return nil
}

// Keep connection alive.
func (c *Connection) keepAlive() {
	ticker := time.NewTicker(keepAliveInterval)
	defer func() {
		ticker.Stop()
		c.getStatus <- SHUTDOWN
	}()

	for {
		select {
		case tick := <-ticker.C:
			err := c.ws.WriteControl(websocket.PingMessage, []byte{}, tick.Add(keepAliveTimeout))
			if err != nil {
				c.pool.client.Errorf("[%s] Tunnel keep-alive failure: %v", c.id, err)
			}
		case status, ok := <-c.setStatus:
			if !ok {
				return
			}

			if status == SHUTDOWN {
				ticker.Stop()
				c.getStatus <- SHUTDOWN

				continue
			}

			if status == UNKNOWN { // signal to return status.
				c.getStatus <- c.status
				continue
			}

			c.status = status
		}
	}
}

func (c *Connection) Status() int {
	c.setStatus <- UNKNOWN
	return <-c.getStatus
}

// serve is the main loop, it:
//   - Waits to receive HTTP requests from the Server.
//   - Executes HTTP requests.
//   - Sends HTTP responses back to the Server.
//
// As in the server code there is no buffering of HTTP request/response body.
// As in the server if any error occurs the connection is closed/thrown.
func (c *Connection) serve() {
	defer func() {
		c.pool.Remove(c)
		c.pool.client.Printf("[%s] Disconnecting tunnel @ %s", c.id, c.pool.target)
		// Stop the keep alive ticker.
		close(c.setStatus)
		// Wait for it to finish.
		<-c.getStatus
		// Close the status channel.
		close(c.getStatus)
		// Tell Close() we're done.
		c.wg.Done()
	}()

	for {
		if !c.serveHandler() {
			return // handler had a socket error, close up shop.
		}
	}
}

func (c *Connection) catchPanic() {
	if r := recover(); r != nil {
		// https://github.com/golang/go/blob/b100e127ca0e398fbb58d04d04e2443b50b3063e/src/runtime/chan.go#LL206C15-L206C15
		if err, _ := r.(error); err != nil && !errors.Is(err, net.ErrClosed) { // ignore this specific panic.
			c.pool.client.Errorf("[%s] panic error: %v\n%s", c.id, err, string(debug.Stack()))
		} else if err == nil {
			c.pool.client.Errorf("[%s] panic: %v\n%s", c.id, r, string(debug.Stack()))
		}
	}
}

// serveHandler is the main loop that handles incoming http requests from the server we're connected to.
func (c *Connection) serveHandler() bool {
	defer c.catchPanic()

	// Read request
	c.setStatus <- IDLE

	_, jsonRequest, err := c.ws.ReadMessage()
	if err != nil {
		if !errors.Is(err, net.ErrClosed) {
			c.pool.client.Errorf("[%s] While waiting for a tunnel request: %v", c.id, err)
		}

		return false
	}

	c.setStatus <- RUNNING
	c.pool.Remove(nil) // This triggers the pool to make a new connection.

	httpRequest := new(mulch.HTTPRequest) // Deserialize request.
	if err := json.Unmarshal(jsonRequest, httpRequest); err != nil {
		c.error(fmt.Sprintf("[%s] Deserializing json tunnel request: %s", c.id, err))
		return false
	}

	req := mulch.UnserializeHTTPRequest(httpRequest)
	handler := c.customHandler

	if c.pool.client.Handler == nil {
		handler = c.defaultHandler
		c.pool.client.Printf("[%s] %s %s", c.pool.client.ID, req.Method, req.URL.String())
	}

	// Pipe request body.
	_, bodyReader, err := c.ws.NextReader()
	if err != nil {
		c.pool.client.Errorf("[%s] Getting tunnel response body reader: %v", c.id, err)
		return false
	}

	// Create a "fake" body.
	req.Body = io.NopCloser(bodyReader)
	// Run defaultHandler or customHandler.
	return handler(req)
}

func (c *Connection) defaultHandler(req *http.Request) bool {
	// This is where a local client sends the server's request off to the Internet.
	resp, err := c.pool.client.client.Do(req)
	if err != nil {
		return !c.error(fmt.Sprintf("[%s] Executing tunneled request: %v", c.id, err))
	}

	bodyWriter, err := c.writeResponseHeaders(resp)
	if err != nil {
		c.pool.client.Errorf("[%s] Making request: %v", c.id, err)
		return false
	}

	if _, err := io.Copy(bodyWriter, resp.Body); err != nil {
		c.pool.client.Errorf("[%s] Getting tunnel pipe response body: %v", c.id, err)
		return false
	}

	resp.Body.Close()
	bodyWriter.Close()

	return true
}

func (c *Connection) writeResponseHeaders(resp *http.Response) (io.WriteCloser, error) {
	// This is where we send the Internet's (http request) response back to the server.
	err := c.ws.WriteMessage(websocket.TextMessage, mulch.SerializeHTTPResponse(resp))
	if err != nil {
		return nil, fmt.Errorf("[%s] writing tunnel response: %w", c.id, err)
	}

	// Pipe response body because an io.ReadCloser (http.Body) doesn't get serialized (above).
	bodyWriter, err := c.ws.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return nil, fmt.Errorf("[%s] getting tunnel response body writer: %w", c.id, err)
	}

	return bodyWriter, nil
}

// error is called when an unrecoverable non-socket error happens in the request.
// The two calls to this method are in the methods above.
// Returns true if there's an error writing to the socket.
func (c *Connection) error(msg string) bool {
	c.pool.client.Errorf(msg)

	resp := mulch.NewHTTPResponse(mulch.ClientErrorCode, int64(len(msg)))
	// Write response
	err := c.ws.WriteMessage(websocket.TextMessage, resp)
	if err != nil {
		c.pool.client.Errorf("[%s] Writing tunnel response: %v", c.id, err)
		return true
	}

	// Write response body
	err = c.ws.WriteMessage(websocket.BinaryMessage, []byte(msg))
	if err != nil {
		c.pool.client.Errorf("[%s] Writing tunnel response body: %v", c.id, err)
		return true
	}

	return false
}

// Close the ws/tcp connection.
func (c *Connection) Close() {
	// Stop the keep alive ticker.
	c.setStatus <- SHUTDOWN
	<-c.getStatus
	// Close the websocket connection.
	_ = c.ws.Close()
	// Wait for both go routines to finish.
	c.wg.Wait()
}
