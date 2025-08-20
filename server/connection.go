package server

import (
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ConnectionStatus is an enumeration that represents the status of WebSocket connection.
type ConnectionStatus int

const (
	// Idle state means it is opened but not doing work now.
	// The default value for Connection is Idle.
	Idle   ConnectionStatus = iota
	Busy                    // Unavailable for use.
	Closed                  // Never use again.
)

// Connection manages a single websocket connection from the peer.
// Supports multiple connections from a single peer at the same time (a pool).
type Connection struct {
	connected time.Time
	pool      *Pool // the pool this connection belongs to.
	sock      *websocket.Conn
	status    ConnectionStatus
	idleSince time.Time
	lock      sync.RWMutex
	requests  int
	// nextResponse is the channel to wait for an HTTP response.
	//
	// The `read` function waits to receive the HTTP response as a separate thread reader.
	// (See https://github.com/hgsgtk/wsp/blob/29cc73bbd67de18f1df295809166a7a5ef52e9fa/server/connection.go#L56 )
	//
	// When a "server" thread proxies, it sends the HTTP request to the peer over the WebSocket,
	// and sends the channel of the io.Reader interface (chan io.Reader) that can read the HTTP
	// response to the field `nextResponse`, then waits until the value is written in the channel
	// (chan io.Reader) by another thread "reader".
	//
	// After the thread "reader" detects that the HTTP response from the peer of the WebSocket connection has been written,
	// it sends the value to the channel (chan io.Reader),
	// and the "server" thread can proceed to process the rest of its procedures.
	nextResponse chan chan io.Reader
}

func (c ConnectionStatus) String() string {
	switch c {
	case Idle:
		return "idle"
	case Busy:
		return "busy"
	case Closed:
		return "closed"
	default:
		return "unknown"
	}
}

// NewConnection returns a new Connection.
// Each connection gets a go routine to read (wait for) messages.
func NewConnection(pool *Pool, sock *websocket.Conn) *Connection {
	// Initialize a new Connection.
	conn := &Connection{
		connected:    time.Now(),
		status:       Idle,
		pool:         pool,
		sock:         sock,
		nextResponse: make(chan chan io.Reader),
	}
	// Mark connection as ready for use.
	conn.Give()
	// Start listening for incoming messages over the WebSocket connection.
	go conn.read()

	return conn
}

// read the incoming message from the connection.
// Every connection has a read() method in a go routine.
func (c *Connection) read() {
	defer func() {
		if r := recover(); r != nil {
			c.pool.Errorf("Websocket crash recovered: %s\n%s", r, string(debug.Stack()))
		}

		c.Close("remote hang up")
	}()

	var (
		err    error
		reader io.Reader
		resp   chan io.Reader
	)

	for {
		if c.Status() == Closed {
			return
		}

		// https://godoc.org/github.com/gorilla/websocket#hdr-Control_Messages
		//
		// We need to ensure :
		//  - no concurrent calls to ws.NextReader() / ws.ReadMessage()
		//  - only one reader exists at a time
		//  - wait for reader to be consumed before requesting the next one
		//  - always be reading on the socket to be able to process control messages ( ping / pong / close )

		// We will block here until a message is received or the ws is closed
		if _, reader, err = c.sock.NextReader(); err != nil {
			return
		}

		if c.Status() != Busy {
			// We received a wild unexpected message, just close the connection.
			return
		}

		// When it gets here, it is expected that either a HttpResponse or a HttpResponseBody has been returned.
		//
		// Next, it waits to receive the value from the Connection.proxyRequest function.
		// that is invoked in the "server" thread.
		// https://github.com/hgsgtk/wsp/blob/29cc73bbd67de18f1df295809166a7a5ef52e9fa/server/connection.go#L157
		if resp = <-c.nextResponse; resp == nil {
			return // We have been unlocked by Close().
		}

		// Send the reader back to Connection.proxyRequest.
		resp <- reader

		// Wait for proxyRequest to close the channel.
		// This notifies that it is done with the reader.
		<-resp // Start the loop over, and take back control of the ws reader.
	}
}

func (c *Connection) Status() ConnectionStatus {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.status
}

// Take notifies that this connection is going to be used.
// Returns nil if the connection is busy.
// This gets called at the beginning of an http request.
func (c *Connection) Take() *Connection {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.requests++

	if c.status == Idle {
		c.pool.Debugf("Taking connection from idle buffer pool %s [%s]", c.pool.id, c.sock.RemoteAddr())
		c.status = Busy

		return c
	}

	// This happens once in a while, and is not a real error condition.
	c.pool.Errorf("Tried to Take() invalid connection (%s) from idle buffer pool %s", c.status, c.pool.id)

	return nil
}

// Give a connection back to the idle buffer pool.
// Signals that this connection is ready to be used again.
// This gets called when an http request has completed.
func (c *Connection) Give() {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.status == Closed {
		// This happening, is actually a bug...
		c.pool.Errorf("Tried to Give() closed connection to idle buffer pool %s", c.pool.id)
		return
	}

	c.pool.Debugf("Giving connection to idle buffer pool %s [%s]", c.pool.id, c.sock.RemoteAddr())

	c.idleSince = time.Now()
	c.status = Idle

	// Avoid blocking on the channel write below, or the server deadlocks.
	j := cap(c.pool.idle)
	if i := len(c.pool.idle); i >= j {
		c.close(fmt.Sprintf("idle buffer pool %d at capacity %d, too many connections", i, j))
		return
	}

	// Stick this connection into the idle buffer pool.
	c.pool.idle <- c
}

// Close the connection.
func (c *Connection) Close(reason string) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.close(reason)
}

// Close the connection (without lock).
func (c *Connection) close(reason string) {
	if c.status == Closed {
		return
	}

	c.pool.Printf("Closing connection from %s [%s], connected: %s, requests: %d, reason: %s",
		c.pool.id, c.sock.RemoteAddr(), time.Since(c.connected).Round(time.Second), c.requests, reason)
	// Unlock a possible wild read() message.
	close(c.nextResponse)
	// Close the underlying TCP connection.
	c.sock.Close()
	// This must be executed *before* lock.Unlock().
	c.status = Closed
}
