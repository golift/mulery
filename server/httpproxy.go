package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"

	"github.com/gorilla/websocket"
	"golift.io/mulery/mulch"
)

/* All of this code is related. One entry point into this file. */

func (c *Connection) catchProxyPanic() {
	if r := recover(); r != nil {
		// https://github.com/golang/go/blob/b100e127ca0e398fbb58d04d04e2443b50b3063e/src/runtime/chan.go#LL206C15-L206C15
		if err, _ := r.(error); err != nil && err.Error() != "send on closed channel" { // ignore this specific panic.
			c.pool.Errorf("panic error: %v\n%s", err, string(debug.Stack()))
		} else if err == nil {
			c.pool.Errorf("panic: %v\n%s", r, string(debug.Stack()))
		}
	}
}

// getNextResponse waits for another upstream response, or for the client to give up.
func (c *Connection) getNextResponse(ctx context.Context, ioCh chan io.Reader) error {
	defer c.catchProxyPanic()

	for {
		select {
		case c.nextResponse <- ioCh:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("http client gave up waiting for remote: %w", ctx.Err())
		}
	}
}

// proxyRequest is the entry point.
// Proxies an HTTP request back through the incoming websocket connection.
func (c *Connection) proxyRequest(resp http.ResponseWriter, req *http.Request) error {
	// Step 1.
	if err := c.sendProxyRequestBody(req); err != nil {
		return err
	}

	// Step 2.
	jsonResponse, err := c.getProxyResponse(req)
	if err != nil {
		return err
	}

	// Step 3.
	if err := c.sendResponseToClient(resp, jsonResponse); err != nil {
		return err
	}

	// Step 4.
	if err := c.copyProxyResponseBody(resp, req); err != nil {
		return err
	}

	// Notify read() that we are done reading the response body, and this connection can be reused.
	c.Give()

	return nil
}

// sendProxyRequestBody is step 1.
func (c *Connection) sendProxyRequestBody(req *http.Request) error {
	defer c.catchProxyPanic()

	jsonReq, err := json.Marshal(mulch.SerializeHTTPRequest(req))
	if err != nil {
		return fmt.Errorf("serializing request: %w", err)
	}

	// Send the serialized HTTP request to the peer.
	if err := c.sock.WriteMessage(websocket.TextMessage, jsonReq); err != nil {
		return fmt.Errorf("writing request: %w", err)
	}

	// Pipe the HTTP request body to the peer.
	bodyWriter, err := c.sock.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return fmt.Errorf("request body writer: %w", err)
	}

	if _, err := io.Copy(bodyWriter, req.Body); err != nil {
		return fmt.Errorf("copying request body: %w", err)
	}

	if err := bodyWriter.Close(); err != nil {
		return fmt.Errorf("closing request body: %w", err)
	}

	return nil
}

// getProxyResponse is step 2.
func (c *Connection) getProxyResponse(req *http.Request) ([]byte, error) {
	defer c.catchProxyPanic()

	responseChannel := make(chan (io.Reader))
	// Notify the read() goroutine that we are done reading the response.
	defer close(responseChannel)

	if err := c.getNextResponse(req.Context(), responseChannel); err != nil {
		return nil, err
	}

	responseReader := <-responseChannel
	if responseReader == nil {
		return nil, fmt.Errorf("%w: no http response reader", ErrInvalidData)
	}

	// Read the HTTP response from the peer.
	jsonResponse, err := io.ReadAll(responseReader)
	if err != nil {
		return nil, fmt.Errorf("reading http response: %w", err)
	}

	return jsonResponse, nil
}

// sendResponseToClient is step 3.
func (c *Connection) sendResponseToClient(resp http.ResponseWriter, jsonResponse []byte) error {
	// Deserialize the HTTP Response.
	httpResponse := new(mulch.HTTPResponse)
	if err := json.Unmarshal(jsonResponse, httpResponse); err != nil {
		return fmt.Errorf("unserializing http response: %w", err)
	}

	// Write response headers back to the client.
	for header, values := range httpResponse.Header {
		for _, value := range values {
			resp.Header().Add(header, value)
		}
	}

	resp.WriteHeader(httpResponse.StatusCode)

	return nil
}

// copyProxyResponseBody is step 4.
func (c *Connection) copyProxyResponseBody(resp http.ResponseWriter, req *http.Request) error {
	defer c.catchProxyPanic()

	// Get the HTTP Response body from the peer.
	// Send a new channel to the read() goroutine to get the next message reader.
	responseBodyChannel := make(chan (io.Reader))
	defer close(responseBodyChannel)

	if err := c.getNextResponse(req.Context(), responseBodyChannel); err != nil {
		return err
	}

	responseBodyReader := <-responseBodyChannel
	if responseBodyReader == nil {
		return fmt.Errorf("%w: no http response body reader", ErrInvalidData)
	}

	// Pipe the HTTP response body right from the remote Proxy to the client.
	if _, err := io.Copy(resp, responseBodyReader); err != nil {
		return fmt.Errorf("copying response body: %w", err)
	}

	return nil
}
