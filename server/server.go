package server

import (
	"errors"
	"fmt"
	"time"

	"golift.io/mulery/mulch"
)

var (
	ErrInvalidKey    = errors.New("invalid secret key provided")
	ErrNoClientID    = errors.New("required client id header is missing")
	ErrNoProxyTarget = errors.New("no proxy target found for request")
	ErrInvalidData   = errors.New("invalid data received")
)

// StartDispatcher dispatches connections from available pools to client requests.
// You need to start this in a go routine.
func (s *Server) StartDispatcher() {
	defer s.shutdown() // close(s.dispatcher)

	const cleanInterval = 5 * time.Second

	cleaner := time.NewTicker(cleanInterval)
	defer cleaner.Stop()

	for threadID := s.Config.Dispatchers; threadID > 0; threadID-- {
		go func(threadID uint) {
			for r := range s.dispatcher {
				s.dispatchRequest(r, threadID)
			}

			// notify shutdown() that dispatcher is closed.
			s.getPool <- nil
		}(threadID)
	}

	for {
		// Runs in an infinite loop:
		// - Checks for done channel closing.
		// - Runs cleaner every 5 seconds.
		select {
		case newPool, ok := <-s.newPool:
			if !ok {
				return
			}

			s.registerPool(newPool)
		case req := <-s.getPool:
			s.threadCount[req.threadID]++
			s.repPool <- s.pools[req.clientID]
		case now := <-cleaner.C:
			s.cleanPools(now)
		case clientID := <-s.getStats:
			s.repStats <- &Stats{
				Pools:   s.poolStats(clientID),
				Threads: s.threadStats(),
			}
		}
	}
}

// poolStats provides data about running pools and connections.
// Useful for a web handler to show an operator what's happening.
func (s *Server) poolStats(cID clientID) map[clientID]any {
	if cID != "" {
		if s.pools[cID] == nil {
			return map[clientID]any{"id not found": nil}
		}

		return map[clientID]any{cID: s.pools[cID].size(time.Now())}
	}

	now := time.Now()

	pools := make(map[clientID]any, len(s.pools))
	for target, pool := range s.pools {
		pools[target] = map[string]any{ // becomes json.
			"connected":    pool.connected,
			"duration":     time.Since(pool.connected).Round(time.Second).String(),
			"idlePoolWait": len(pool.idle),
			"idlePoolSize": cap(pool.idle),
			"client":       pool.handshake,
			"sizes":        pool.size(now),
		}
	}

	return pools
}

func (s *Server) threadStats() map[uint]uint64 {
	threadCount := make(map[uint]uint64, len(s.threadCount))

	for k, v := range s.threadCount {
		threadCount[k] = v
	}

	return threadCount
}

// cleanPools removes empty Pools; those with no incoming client connections.
// This also shoves pool counters into prometheus if it's enabled.
// It is invoked every 5 sesconds and at shutdown.
func (s *Server) cleanPools(now time.Time) {
	if len(s.pools) == 0 {
		return
	}

	totals := &PoolSize{}
	connsPerPool := make(map[int]int)
	pools := make(map[clientID]*Pool, len(s.pools))

	for target, pool := range s.pools {
		if pool.IsEmpty() {
			s.Config.Logger.Debugf("Removing empty connection pool: %s", pool.id)
			pool.Shutdown()
			s.closed += pool.closed

			continue
		}

		pools[target] = pool
		ps := pool.Size(now)
		totals.Total += ps.Total
		totals.Idle += ps.Idle
		totals.Busy += ps.Busy
		totals.Closed += ps.Closed
		connsPerPool[ps.Total]++
	}

	totals.Closed += s.closed
	s.pools = pools
	s.Config.Logger.Debugf("%d pools, %d connections, %d idle, %d busy, %d closed",
		len(s.pools), totals.Total, totals.Idle, totals.Busy, totals.Closed)
	s.saveMetrics(totals, connsPerPool)
}

func (s *Server) saveMetrics(totals *PoolSize, connsPerPool map[int]int) {
	if s.metrics == nil {
		return
	}

	// we have to limit the label values to something...
	const maximum = 11

	for conns, pools := range connsPerPool {
		if conns > maximum {
			connsPerPool[maximum] += pools
		}
	}

	for conns := 1; conns <= maximum; conns++ {
		label := fmt.Sprintf("%02d", conns)
		if conns >= maximum {
			label += "+"
		}

		s.metrics.PoolConns.WithLabelValues(label).Set(float64(connsPerPool[conns]))
	}

	s.metrics.Conns.WithLabelValues("total").Set(float64(totals.Total))
	s.metrics.Conns.WithLabelValues("busy").Set(float64(totals.Busy))
	s.metrics.Conns.WithLabelValues("idle").Set(float64(totals.Idle))
	s.metrics.Conns.WithLabelValues("closed").Set(float64(totals.Closed))
	s.metrics.Pools.Set(float64(len(s.pools)))
}

// dispatchRequest runs every time an http request comes into the server.
// This finds a pool for the request, and sends the request to it.
// So the problem here is that this function is blocking, and it will block
// every single request if there is no available idle connection for the
// current request it's processing. If the clients are slow and the requests
// long this could be problematic. Start more dispatchers if you need to.
func (s *Server) dispatchRequest(request *dispatchRequest, threadID uint) {
	defer close(request.connection)

	for {
		s.Config.Logger.Debugf("[%d] dispatchRequest: 1 ask %s", threadID, request.client)
		// Ask the main thread for this pool by ID.
		s.getPool <- &getPoolRequest{clientID: request.client, threadID: threadID}
		s.Config.Logger.Debugf("[%d] dispatchRequest: 2 wait %s", threadID, request.client)
		// Get the pool reply from the main thread.
		pool := <-s.repPool
		s.Config.Logger.Debugf("[%d] dispatchRequest: 3 got %s", threadID, request.client)

		if pool == nil {
			s.Config.Logger.Debugf("[%d] dispatchRequest: 4 empty pool %s", threadID, request.client)
			return // no client pool with that name.
		}

		// This blocks until an idle connection is available.
		conn := <-pool.idle
		if conn == nil {
			s.Config.Logger.Debugf("[%d] dispatchRequest: 4 empty conn channel %s", threadID, request.client)
			return // pool was shutdown as request came in.
		}

		s.Config.Logger.Debugf("[%d] dispatchRequest: 4 take %s", threadID, request.client)
		// Verify that we can use this connection and take it.
		if connection := conn.Take(); connection != nil {
			request.connection <- connection
			s.Config.Logger.Debugf("[%d] dispatchRequest: 5 done %s", threadID, request.client)

			return
		}

		s.Config.Logger.Debugf("[%d] dispatchRequest: 5 restart %s", threadID, request.client)
	}
}

// Register the connection into server pools.
// This is called through a channel from the register handler.
func (s *Server) registerPool(client *PoolConfig) {
	cID := mulch.HashKeyID(client.secret, client.ID)
	if pool := s.pools[clientID(cID)]; pool == nil {
		s.pools[clientID(cID)] = NewPool(s, client, cID+" ["+client.Name+"]")
	}

	// Add the WebSocket connection to the pool
	s.pools[clientID(cID)].Register(client.Sock)
}

// Shutdown stops the Server.
func (s *Server) Shutdown() {
	// closing this channel makes shutdown() run.
	close(s.newPool)
}

func (s *Server) shutdown() {
	close(s.dispatcher)

	for i := s.Config.Dispatchers; i > 0; i-- {
		<-s.getPool // wait for dispatchers to finish.
	}

	close(s.getPool)
	close(s.repPool)
	close(s.getStats)
	close(s.repStats)

	for target, pool := range s.pools {
		pool.Shutdown()
		delete(s.pools, target)
	}
}
