package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// NewVhost creates an empty RabbitMQ vhost for one test, deletes it when the
// test ends, and returns an AMQP URL for it. Queues belong to a vhost, so
// tests (in any package, running at the same time) never see each other's
// messages, and every test starts with no topology at all.
//
// It needs TEST_AMQP_URL (whose user must be a RabbitMQ administrator) and
// TEST_RABBITMQ_API_URL, the management API; `make test` sets both. With
// -short, integration tests are skipped instead.
func NewVhost(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs RabbitMQ (run make test)")
	}
	amqpURL, api := os.Getenv("TEST_AMQP_URL"), os.Getenv("TEST_RABBITMQ_API_URL")
	if amqpURL == "" || api == "" {
		// Fail rather than skip, so a misconfigured `make test` can't pass silently.
		t.Fatal("TEST_AMQP_URL and TEST_RABBITMQ_API_URL must be set: run the suite with `make test`, or `go test -short` for unit tests only")
	}

	u, err := url.Parse(amqpURL)
	if err != nil {
		t.Fatalf("parse TEST_AMQP_URL: %v", err)
	}
	m := Management{API: strings.TrimSuffix(api, "/"), User: u.User.Username()}
	m.Password, _ = u.User.Password()

	name := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	m.Vhost = name
	m.do(t, http.MethodPut, "/api/vhosts/"+name, nil)
	m.do(t, http.MethodPut, "/api/permissions/"+name+"/"+url.PathEscape(m.User),
		map[string]string{"configure": ".*", "write": ".*", "read": ".*"})
	t.Cleanup(func() { m.do(t, http.MethodDelete, "/api/vhosts/"+name, nil) })

	u.Path = "/" + name
	return u.String()
}

// Management is a minimal client for the RabbitMQ management API, scoped to
// one vhost. Tests use it to inspect what the AMQP protocol doesn't expose,
// such as a queue's type and arguments.
type Management struct {
	API, User, Password, Vhost string
}

// ManagementFor returns a client for the vhost in amqpURL (from NewVhost).
func ManagementFor(t *testing.T, amqpURL string) Management {
	t.Helper()
	u, err := url.Parse(amqpURL)
	if err != nil {
		t.Fatalf("parse AMQP URL: %v", err)
	}
	m := Management{
		API:   strings.TrimSuffix(os.Getenv("TEST_RABBITMQ_API_URL"), "/"),
		User:  u.User.Username(),
		Vhost: strings.TrimPrefix(u.Path, "/"),
	}
	m.Password, _ = u.User.Password()
	return m
}

// QueueInfo is the part of a queue's management representation tests check.
type QueueInfo struct {
	Type      string         `json:"type"`
	Durable   bool           `json:"durable"`
	Arguments map[string]any `json:"arguments"`
}

// Queue returns the queue's type, durability and arguments.
func (m Management) Queue(t *testing.T, name string) QueueInfo {
	t.Helper()
	var q QueueInfo
	if err := json.Unmarshal(m.do(t, http.MethodGet, "/api/queues/"+url.PathEscape(m.Vhost)+"/"+url.PathEscape(name), nil), &q); err != nil {
		t.Fatalf("decode queue %s: %v", name, err)
	}
	return q
}

func (m Management) do(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, m.API+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(m.User, m.Password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("management API %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("management API %s %s: %s", method, path, fmt.Sprintf("%d %s", resp.StatusCode, out))
	}
	return out
}
