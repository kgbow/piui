package store

import (
	"errors"
	"strings"
	"sync"

	"github.com/egoist/mygo"
)

// ErrSecretNotFound indicates no credential is stored under the requested key.
var ErrSecretNotFound = errors.New("secret not found")

// SecretStore abstracts OS-native credential storage (macOS Keychain, Linux Secret Service,
// Windows Credential Manager) so production runs never persist plaintext API keys to disk,
// while unit tests and headless environments remain hermetic and non-blocking.
type SecretStore interface {
	Name() string
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

var (
	secretStoreMu     sync.RWMutex
	activeSecretStore SecretStore
)

// SetSecretStore installs the process-wide SecretStore (called by main.go on startup,
// or by unit tests with NewMemorySecretStore). Passing nil disables external secret storage.
func SetSecretStore(s SecretStore) {
	secretStoreMu.Lock()
	defer secretStoreMu.Unlock()
	activeSecretStore = s
}

// ActiveSecretStore returns the currently configured SecretStore, or nil if none is active.
func ActiveSecretStore() SecretStore {
	secretStoreMu.RLock()
	defer secretStoreMu.RUnlock()
	return activeSecretStore
}

const KeychainServiceName = "dev.piui.credentials"

// SystemKeychainStore adapts mygo.Secrets (OS Keychain / Secret Service / Credential Manager).
type SystemKeychainStore struct {
	mod mygo.SecretsModule
}

// NewSystemKeychainStore returns a SecretStore backed by the OS credential manager.
func NewSystemKeychainStore() *SystemKeychainStore {
	return &SystemKeychainStore{
		mod: mygo.Secrets.Service(KeychainServiceName),
	}
}

func (s *SystemKeychainStore) Name() string { return "system-keychain" }

func (s *SystemKeychainStore) Get(key string) (string, error) {
	b, err := s.mod.Get(profileSecretAccountKey(key))
	if err != nil {
		if errors.Is(err, mygo.ErrSecretNotFound) {
			return "", ErrSecretNotFound
		}
		return "", err
	}
	return string(b), nil
}

func (s *SystemKeychainStore) Set(key, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return s.Delete(key)
	}
	return s.mod.Set(profileSecretAccountKey(key), []byte(value))
}

func (s *SystemKeychainStore) Delete(key string) error {
	return s.mod.Delete(profileSecretAccountKey(key))
}

func profileSecretAccountKey(profileID string) string {
	return "profile-apikey:" + strings.TrimSpace(profileID)
}

// MemorySecretStore is an in-memory SecretStore for deterministic testing.
type MemorySecretStore struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewMemorySecretStore creates an isolated in-memory SecretStore.
func NewMemorySecretStore() *MemorySecretStore {
	return &MemorySecretStore{data: make(map[string]string)}
}

func (m *MemorySecretStore) Name() string { return "memory-keychain" }

func (m *MemorySecretStore) Get(key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[profileSecretAccountKey(key)]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (m *MemorySecretStore) Set(key, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return m.Delete(key)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[profileSecretAccountKey(key)] = value
	return nil
}

func (m *MemorySecretStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, profileSecretAccountKey(key))
	return nil
}
