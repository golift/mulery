package client

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Pool of connections to a remote Server.
type Pool struct {
	client      *Client
	target      string
	secretKey   string
	connections []*Connection
	disconnects int
	done        chan bool
	getSize     chan struct{}
	repSize     chan *PoolSize
	conChan     chan *Connection
	repChan     chan struct{}
	connSnap    chan []*Connection
	shutdown    atomic.Bool
	shutdownNow sync.Once
	lastTry     time.Time
	backOff     time.Duration
	ticker      *time.Ticker
}

// PoolSize represent the number of open connections per status.
type PoolSize struct {
	Disconnects int       `json:"disconnects"`
	Connecting  int       `json:"connecting"`
	Idle        int       `json:"idle"`
	Running     int       `json:"running"`
	Total       int       `json:"total"`
	LastConn    time.Time `json:"lastConn"`
	LastTry     time.Time `json:"lastTry"`
	Active      bool      `json:"active"`
}

// StartPool creates and starts a pool in one command.
func StartPool(ctx context.Context, client *Client, target string, secretKey string) *Pool {
	pool := NewPool(client, target, secretKey)
	pool.Start(ctx)

	return pool
}

// NewPool creates a new Pool.
func NewPool(client *Client, target string, secretKey string) *Pool {
	return &Pool{
		client:      client,
		target:      target,
		secretKey:   secretKey,
		connections: []*Connection{},
		done:        make(chan bool),
		getSize:     make(chan struct{}),
		repSize:     make(chan *PoolSize),
		conChan:     make(chan *Connection),
		repChan:     make(chan struct{}),
		connSnap:    make(chan []*Connection, 1),
		backOff:     time.Second,
		ticker:      time.NewTicker(client.CleanInterval),
	}
}

// Start connects to the remote server and runs a ticker loop to maintain the connection.
func (p *Pool) Start(ctx context.Context) {
	p.connector(ctx, time.Now())

	go func() {
		p.ticker.Reset(p.client.CleanInterval)

		defer func() {
			close(p.getSize)
			close(p.repSize)
			close(p.conChan)
			close(p.repChan)
			close(p.connSnap)
			close(p.done)
		}()

		for {
			select {
			case now := <-p.ticker.C:
				if p.connector(ctx, now) {
					p.ticker.Stop()
					p.client.restart(ctx)
				}
			case <-p.getSize:
				p.repSize <- p.size()
			case conn := <-p.conChan:
				p.remove(conn)
				p.repChan <- struct{}{}
			case exit := <-p.done:
				p.ticker.Stop()
				p.done <- true

				if exit {
					return
				}
				// Create a copy that can be used to shutdown all the connections without returning our pointer.
				snapshot := make([]*Connection, len(p.connections))
				copy(snapshot, p.connections)
				p.connSnap <- snapshot
			}
		}
	}()
}

// The garbage collector runs every second or so.
// If the size of the pool is not equivalent to the desired size,
// then N go functions are created that add additional pool connections.
// If the connection fails, the connection is removed from the pool.
func (p *Pool) connector(ctx context.Context, now time.Time) bool {
	if p.backOff > p.client.MaxBackoff {
		p.backOff = p.client.BackoffReset // keep bringing it back down.
	}

	if now.Sub(p.lastTry) < p.backOff {
		return false
	}

	p.lastTry = now
	poolSize := p.size()
	// Create enough connection to fill the pool.
	toCreate := p.client.PoolIdleSize - poolSize.Idle

	// Create only one connection if the pool is empty.
	if poolSize.Total == 0 && toCreate < 1 {
		toCreate = 1
	}

	// Open at most PoolMaxSize connections.
	if poolSize.Total+toCreate > p.client.PoolMaxSize {
		toCreate = p.client.PoolMaxSize - poolSize.Total
	}

	return p.fillConnectionPool(ctx, now, toCreate)
}

func (p *Pool) fillConnectionPool(ctx context.Context, now time.Time, toCreate int) bool {
	if p.client.RoundRobinConfig != nil {
		if toCreate == 0 || len(p.connections) > 0 {
			// Keep this up to date, or the logic will skip to the next server prematurely.
			p.client.lastConn = now
		} else if now.Sub(p.client.lastConn) > p.client.RetryInterval {
			// We need more connections and the last successful connection was too long ago.
			// Restart and skip to the next server in the round robin target list.
			return true
		}
	}

	// Try to reach ideal pool size.
	for ; toCreate > 0; toCreate-- {
		// This is the only place a connection is added to the pool.
		conn := NewConnection(p)
		if err := conn.Connect(ctx); err != nil {
			p.client.Errorf("Connecting to tunnel @ %s: %s", p.target, err)
			p.backOff += p.client.Backoff

			break // don't try any more this round.
		}

		p.connections = append(p.connections, conn)
		p.backOff = p.client.Backoff
	}

	return false
}

// Remove a connection from the pool.
func (p *Pool) Remove(conn *Connection) {
	if !p.shutdown.Load() {
		p.conChan <- conn
		<-p.repChan
	}
}

func (p *Pool) remove(connection *Connection) {
	var filtered []*Connection // == nil

	for _, conn := range p.connections {
		if connection != conn {
			filtered = append(filtered, conn)
		} else {
			p.disconnects++
		}
	}

	p.connections = filtered
}

// Shutdown and close all connections in the pool.
// Safe to call concurrently and multiple times.
func (p *Pool) Shutdown() {
	p.shutdownNow.Do(func() {
		// Send a signal to the connector to stop the ticker.
		p.done <- false
		<-p.done

		// Receive a snapshot of connections from the pool goroutine.
		// This avoids a data race on p.connections (owned by the pool goroutine).
		conns := <-p.connSnap

		// Set shutdown before closing connections so Remove() skips the channel send.
		p.shutdown.Store(true)

		for _, conn := range conns {
			conn.Close()
		}

		// Signal the pool goroutine to exit.
		p.done <- true
		<-p.done
	})
}

func (ps *PoolSize) String() string {
	return fmt.Sprintf("Connecting %d, idle %d, running %d, total %d",
		ps.Connecting, ps.Idle, ps.Running, ps.Total)
}

// Size returns the current telemetric state of the pool.
func (p *Pool) Size() *PoolSize {
	p.getSize <- struct{}{}
	return <-p.repSize
}

func (p *Pool) size() *PoolSize {
	poolSize := new(PoolSize)
	poolSize.Total = len(p.connections)
	poolSize.Disconnects = p.disconnects
	poolSize.LastTry = p.lastTry
	poolSize.Active = !p.shutdown.Load()

	if poolSize.LastConn = p.lastTry; !p.shutdown.Load() && p.client.RoundRobinConfig != nil {
		poolSize.LastConn = p.client.lastConn
	}

	if p.shutdown.Load() {
		return poolSize
	}

	for _, connection := range p.connections {
		switch connection.Status() {
		case CONNECTING:
			poolSize.Connecting++
		case IDLE:
			poolSize.Idle++
		case RUNNING:
			poolSize.Running++
		}
	}

	return poolSize
}
