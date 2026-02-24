package server

import (
	"time"

	"github.com/gorilla/websocket"
	"golift.io/mulery/mulch"
)

// Pool handles all connections from the peer.
// Each pool is unique by it's clientID.
type Pool struct {
	connected   time.Time
	handshake   *mulch.Handshake
	minSize     int
	idleTimeout time.Duration
	id          string
	connections []*Connection
	closed      int
	idle        chan *Connection
	newConn     chan *Connection
	askClean    chan struct{}
	askSize     chan time.Time
	getSize     chan *PoolSize
	mulch.Logger
	metrics *Metrics
}

// clientID represents the identifier of the connected WebSocket client.
type clientID string

// NewPool creates a new Pool, and starts one go routine per pool to keep it clean and running.
// Each pool represents 1 client, and each client may have many connections.
func NewPool(server *Server, client *PoolConfig, altID string) *Pool {
	if altID == "" {
		altID = client.ID
	}

	// update pool size; we add 1 so the pool may have 1 threads more than it's minimum idle.
	pool := &Pool{
		connected:   time.Now(),
		handshake:   client.Handshake,
		id:          altID,
		minSize:     client.Size + 1, // This 1 allows slightly less thread teardown/bringup.
		idle:        make(chan *Connection, client.MaxSize+1),
		idleTimeout: server.Config.IdleTimeout,
		newConn:     make(chan *Connection),
		askClean:    make(chan struct{}),
		askSize:     make(chan time.Time),
		getSize:     make(chan *PoolSize),
		Logger:      server.Config.Logger,
		metrics:     server.metrics,
	}

	go pool.keepRunning() // gofunc:3 (N)

	return pool
}

func (pool *Pool) shutdown() {
	pool.Debugf("Shutting down pool: %v", pool.id)
	defer pool.Debugf("Done shutting down pool: %v", pool.id)

	for _, connection := range pool.connections {
		connection.Close("shutdown")
	}

	close(pool.askClean)
	close(pool.askSize)
	close(pool.getSize)
	close(pool.idle)
}

func (pool *Pool) keepRunning() {
	defer pool.shutdown()

	for {
		select {
		case <-pool.askClean:
			pool.clean()
			pool.getSize <- &PoolSize{Total: len(pool.connections)} // shoehorn.
		case now := <-pool.askSize:
			pool.getSize <- pool.size(now)
		case conn, ok := <-pool.newConn:
			if !ok {
				return
			}

			pool.clean()
			pool.connections = append(pool.connections, conn)
			pool.Printf("Registering new connection from %s [%s], tunnels: %d, idle: %d/%d",
				pool.id, conn.sock.RemoteAddr(), len(pool.connections), len(pool.idle), cap(pool.idle))
		}
	}
}

// Register creates a new Connection and adds it to the pool.
func (pool *Pool) Register(ws *websocket.Conn) {
	pool.cleanIdleChan()
	pool.newConn <- NewConnection(pool, ws)
}

// clean removes dead and idle connections from the pool.
// Calling pool.IsEmpty is the only way to trigger this.
func (pool *Pool) clean() {
	var (
		idle = 0
		save = []*Connection{}
		keep bool
	)

	for _, connection := range pool.connections {
		if idle, keep = pool.cleanConnection(connection, idle); keep {
			save = append(save, connection)
		} else {
			pool.closed++
		}
	}

	pool.connections = save
}

// cleanIdleChan removes all non-idle connections from the idle channel buffer.
// This should run every time a new connection registers; to clean out old dead connections.
func (pool *Pool) cleanIdleChan() {
	for i := len(pool.idle); i > 0; i-- {
		if conn := <-pool.idle; conn.Status() == Idle {
			pool.idle <- conn
		}
	}
}

func (pool *Pool) cleanConnection(connection *Connection, idle int) (int, bool) {
	// Ensure a busy connection is never closed.
	connection.lock.Lock()
	defer connection.lock.Unlock()

	if connection.status == Idle {
		idle++
		// Terminate the connection if it is idle since more than IdleTimeout.
		if age := time.Since(connection.idleSince); idle > pool.minSize && age > pool.idleTimeout {
			// We have enough idle connections in the pool, and this one is old.
			pool.Printf("Closing idle connection: %s [%s], tunnels: %d , idle: %d/%d",
				pool.id, connection.sock.RemoteAddr(), len(pool.connections), len(pool.idle), cap(pool.idle))
			connection.close("idle " + age.String())
		}
	}

	return idle, connection.status != Closed
}

// IsEmpty cleans the pool and return true if the pool is empty.
func (pool *Pool) IsEmpty() bool {
	pool.askClean <- struct{}{}
	return (<-pool.getSize).Total == 0
}

// Shutdown closes every connection in the pool and closes all channels.
func (pool *Pool) Shutdown() {
	close(pool.newConn)
	pool.Debugf("called pool Shutdown: %s", pool.id)
}

// PoolSize is the number of connection in each state in the pool.
type PoolSize struct {
	Total  int          `json:"total"`
	Idle   int          `json:"idle"`
	Busy   int          `json:"busy"`
	Closed int          `json:"closed"`
	Conns  []*ConnStats `json:"conns"`
}

type ConnStats struct {
	Remote    string    `json:"remote"`
	Requests  int       `json:"requests"`
	Connected time.Time `json:"conneteed"`
	Idle      string    `json:"idle"`
}

// Size return the number of connection in each state in the pool.
// Uses `now` to calculate how long a connection has been established.
func (pool *Pool) Size(now time.Time) *PoolSize {
	pool.askSize <- now
	return <-pool.getSize
}

// size return the number of connection in each state in the pool.
// Must hold connection read locks when accessing mutable connection fields.
func (pool *Pool) size(now time.Time) *PoolSize {
	size := PoolSize{
		Total:  len(pool.connections),
		Closed: pool.closed,
		Conns:  make([]*ConnStats, len(pool.connections)),
	}

	for idx, connection := range pool.connections {
		connection.lock.RLock()
		size.Conns[idx] = &ConnStats{
			Remote:    connection.sock.RemoteAddr().String(),
			Connected: connection.connected,
			Requests:  connection.requests,
			Idle:      now.Sub(connection.idleSince).Round(time.Second).String(),
		}

		switch connection.status {
		case Idle:
			size.Idle++
		case Busy:
			size.Busy++
		}

		connection.lock.RUnlock()
	}

	return &size
}
