// Package store persists controller state (cluster keys, enrolled nodes,
// join tokens, settings) as a single JSON document written atomically.
// The controller is designed so that losing this file is recoverable —
// nodes simply re-enroll — but keeping it makes restarts seamless.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
)

// PersistedNode is the durable part of a node's identity.
type PersistedNode struct {
	ID         string            `json:"id"`
	Kind       protocol.NodeKind `json:"kind"`
	Name       string            `json:"name"`
	PublicURL  string            `json:"public_url"`
	Region     string            `json:"region"`
	Capacity   int               `json:"capacity_conns"`
	SecretHash string            `json:"secret_hash"` // sha256 hex of node secret
	JoinedAt   time.Time         `json:"joined_at"`
}

// JoinToken is a persisted enrollment token. The token value itself is
// stored hashed; the plaintext is only shown once at creation.
type JoinToken struct {
	ID        string     `json:"id"`
	TokenHash string     `json:"token_hash"`
	Note      string     `json:"note,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	MaxUses   int        `json:"max_uses,omitempty"`
	Uses      int        `json:"uses"`
}

// State is the full persisted document.
type State struct {
	AdminKeyHash string            `json:"admin_key_hash"`
	SigningKey   string            `json:"signing_key"`
	IngestKey    string            `json:"ingest_key"`
	Settings     protocol.Settings `json:"settings"`
	Tokens       []JoinToken       `json:"tokens"`
	Nodes        []PersistedNode   `json:"nodes"`
}

// Store wraps State with locking and atomic persistence.
type Store struct {
	mu    sync.Mutex
	path  string
	state State
}

// Hash returns the hex sha256 of a credential for at-rest storage.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Open loads (or initializes) the state file under dataDir.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("store: creating data dir: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, "state.json")}
	b, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.state); err != nil {
			return nil, fmt.Errorf("store: corrupt state file %s: %w", s.path, err)
		}
	case os.IsNotExist(err):
		s.state.Settings = protocol.DefaultSettings()
	default:
		return nil, fmt.Errorf("store: reading state: %w", err)
	}
	return s, nil
}

// save writes the state atomically. Callers must hold s.mu.
func (s *Store) save() error {
	b, err := json.MarshalIndent(&s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Mutate runs fn with exclusive access to the state and persists after.
func (s *Store) Mutate(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	return s.save()
}

// View runs fn with read access to a consistent state snapshot.
func (s *Store) View(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
}
