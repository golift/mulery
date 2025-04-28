package client

import (
	"fmt"
	"io"
	"net/http"
	"sync"
)

/* This file turns http.ResponseWriter into an http.Response. */

type req2Handler struct {
	req  *http.Request
	resp *http.Response
	conn *Connection
	body io.WriteCloser
	mu   sync.Mutex
	err  error
}

// customHandler builds the logic to convert an http.ResponseWriter into an http.Response.
// Returns true on success (ok), false on error.
func (c *Connection) customHandler(req *http.Request) bool {
	writer := &req2Handler{
		req:  req,
		resp: &http.Response{Header: make(http.Header)},
		conn: c,
	}

	c.pool.client.Handler(writer, req)

	if writer.body != nil {
		writer.body.Close()
	}

	return writer.err == nil
}

// Write satisfies the ResponseWriter interface and handles
// transporting the content from the upstream to the downstream.
func (r *req2Handler) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.body == nil {
		r.WriteHeader(http.StatusOK)
	}

	if r.body == nil {
		return 0, fmt.Errorf("[%s] tunnel write failed: %w", r.conn.id, r.err)
	}

	size, err := r.body.Write(data)
	r.resp.ContentLength += int64(size)

	if err != nil {
		r.err = err
		return size, fmt.Errorf("[%s] tunnel write failed: %w", r.conn.id, err)
	}

	return size, nil
}

// WriteHeader satisfies the ResponseWriter interface and sends the response
// body off to the server.
func (r *req2Handler) WriteHeader(statusCode int) {
	r.resp.StatusCode = statusCode
	r.resp.Status = http.StatusText(statusCode)
	r.body, r.err = r.conn.writeResponseHeaders(r.resp)
}

// Header returns the response headers.
func (r *req2Handler) Header() http.Header {
	return r.resp.Header
}

// Make sure the req2Handler satisfies the ResponseWriter interface.
var _ = http.ResponseWriter(&req2Handler{})
