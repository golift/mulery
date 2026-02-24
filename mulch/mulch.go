// Package mulch provides shared methods, structures and variables
// used by mulery client library, server library and server application.
package mulch

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const SecretKeyHeader = "x-secret-key"

type Handshake struct {
	Size     int    `json:"size"`     // idle connections.
	MaxSize  int    `json:"max"`      // buffer pool size.
	ID       string `json:"id"`       // client ID
	Name     string `json:"name"`     // For logs only.
	Compress string `json:"compress"` // gzip, bzip, etc, not used yet.
	// ClientIDs is for you to identify your clients with your own ID(s).
	ClientIDs []any `json:"clientIds"`
}

const HandshakeTimeout = 15 * time.Second

// HashKeyID creates a unique client ID hash.
// If a custom key validator returns a secret(string),
// hash that with the client id to create a new client id.
// This is custom logic you probably don't need, and you can
// avoid it by returning an empty string from the custom key validator.
func HashKeyID(secret string, clientID string) string {
	if secret == "" {
		return clientID
	}

	hash := sha256.New()
	hash.Write([]byte(secret + clientID))

	return hex.EncodeToString(hash.Sum(nil))
}
