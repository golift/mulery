package client

import (
	"fmt"
	"io"
	"net/http"
	"sync"
)

// ErrNilBody probably never happens, but it's better than a panic in case it does.
var ErrNilBody = fmt.Errorf("got Write before WriteHeaders, body is nil")

type req2Handler struct {
	req  *http.Request
	resp *http.Response
	conn *Connection
	body io.WriteCloser
	mu   sync.Mutex
	err  bool
}

// customHandler builds the logic to convert an http.ResponseWriter into an http.Response.
func (connection *Connection) customHandler(req *http.Request) bool {
	writer := &req2Handler{
		req:  req,
		resp: &http.Response{Header: make(http.Header)},
		conn: connection,
	}

	connection.pool.client.Config.Handler(writer, req)
	writer.body.Close()

	return !writer.err
}

// Write satisfies the ResponseWriter interface and handles
// transporting the content from the upstream to the downstream.
func (r *req2Handler) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.body == nil {
		r.err = true
		return 0, ErrNilBody
	}

	size, err := r.body.Write(data)
	r.resp.ContentLength += int64(size)

	if err != nil {
		r.err = true
		return size, fmt.Errorf("tunnel write failed: %w", err)
	}

	return size, nil
}

// WriteHeader satisfies the ResponseWriter interface and sends the response
// body off to the server.
func (r *req2Handler) WriteHeader(statusCode int) {
	r.resp.StatusCode = statusCode
	r.resp.Status = http.StatusText(statusCode)
	r.body = r.conn.writeResponseHeaders(r.resp)
}

// Header returns the response headers.
func (r *req2Handler) Header() http.Header {
	return r.resp.Header
}

// Make sure the req2Handler satisfies the ResponseWriter interface.
var _ = http.ResponseWriter(&req2Handler{})