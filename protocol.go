package cmdc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"time"
)

// Stream identifies which output stream a chunk was written to.
type Stream int

const (
	Stdout Stream = 1
	Stderr Stream = 2
)

// Chunk is a piece of command output. A cached result is a sequence of
// chunks so that stdout and stderr can be replayed in the recorded order.
type Chunk struct {
	Stream Stream `json:"s"`
	Data   []byte `json:"d"`
}

// Operations of the protocol between the client and the daemon.
// Messages are newline delimited JSON over a unix domain socket.
const (
	OpHello  = "hello"
	OpGet    = "get"
	OpSet    = "set"
	OpStatus = "status"
	OpClear  = "clear"
	OpStop   = "stop"
)

// Request is a message from the client to the daemon.
type Request struct {
	Op      string        `json:"op"`
	Key     string        `json:"key,omitempty"`
	Label   string        `json:"label,omitempty"`
	Expires time.Duration `json:"expires,omitempty"`
	Chunks  []Chunk       `json:"chunks,omitempty"`
}

// Response is a message from the daemon to the client.
type Response struct {
	Error   string  `json:"error,omitempty"`
	Version string  `json:"version,omitempty"`
	PID     int     `json:"pid,omitempty"`
	Hit     bool    `json:"hit,omitempty"`
	Stored  bool    `json:"stored,omitempty"`
	Cleared int     `json:"cleared,omitempty"`
	Chunks  []Chunk `json:"chunks,omitempty"`
	Status  *Status `json:"status,omitempty"`
}

// Status describes the state of the daemon.
type Status struct {
	Running   bool         `json:"running"`
	Socket    string       `json:"socket"`
	PID       int          `json:"pid,omitempty"`
	Version   string       `json:"version,omitempty"`
	Uptime    string       `json:"uptime,omitempty"`
	Entries   int          `json:"entries"`
	TotalSize int64        `json:"total_size"`
	Items     []StatusItem `json:"items,omitempty"`
}

// StatusItem describes a cached entry.
type StatusItem struct {
	Command   string `json:"command"`
	Size      int64  `json:"size"`
	Age       string `json:"age"`
	ExpiresIn string `json:"expires_in"`
}

// CacheKey returns the cache key for a command line. The values of the
// environment variables named in keyEnv are part of the key; the working
// directory and other environment variables are not.
func CacheKey(argv []string, keyEnv []string) string {
	type envVar struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Set   bool   `json:"set"`
	}
	names := slices.Clone(keyEnv)
	slices.Sort(names)
	names = slices.Compact(names)
	env := make([]envVar, 0, len(names))
	for _, name := range names {
		v, ok := os.LookupEnv(name)
		env = append(env, envVar{Name: name, Value: v, Set: ok})
	}
	b, _ := json.Marshal(struct {
		Argv []string `json:"argv"`
		Env  []envVar `json:"env"`
	}{argv, env})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
