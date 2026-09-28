package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

// The in-repo race demonstration (DESIGN.md §11): real concurrent HTTP
// requests against a real server and database, all released at once behind a
// barrier, with exact outcome counts asserted. Each scenario runs several
// rounds on fresh tenants, since a single lucky interleaving proves little.
//
// These tests are not t.Parallel: each opens up to raceConns database
// connections, and running them one at a time keeps the total well under
// Postgres's default limit of 100.

const (
	raceRounds = 3
	// raceConns sizes the database pool so that most racing requests hold a
	// connection at the same time: the contention happens in Postgres (row
	// locks, the unique index), not in a queue for the Go-side pool.
	raceConns = 25
)

func TestRaceConcurrentCreatesSameSlug(t *testing.T) {
	const n = 50
	rs := newRaceServer(t)

	for round := range raceRounds {
		slug := fmt.Sprintf("race-create-%d", round)
		body := fmt.Sprintf(`{"slug":%q,"name":"Racer"}`, slug)

		results := rs.race(t, n, func(int) *http.Request {
			return rs.request(t, http.MethodPost, "/v1/tenants", body)
		})

		assertOutcomes(t, results, map[outcome]int{
			{http.StatusCreated, ""}: 1,
			{http.StatusConflict, string(domain.CodeTenantAlreadyExists)}: n - 1,
		})

		// Exactly one tenant, one deploy task and one event were written.
		var tenantID uuid.UUID
		rs.queryRow(t, `SELECT id FROM tenants WHERE slug = $1`, slug).Scan(&tenantID)
		rs.assertCount(t, 1, `SELECT count(*) FROM tenants WHERE slug = $1`, slug)
		rs.assertCount(t, 1, `SELECT count(*) FROM tasks WHERE tenant_id = $1`, tenantID)
		rs.assertCount(t, 1, `SELECT count(*) FROM outbox o JOIN tasks t ON t.id = o.task_id WHERE t.tenant_id = $1`, tenantID)
	}
}

func TestRaceConcurrentPatchesSameVersion(t *testing.T) {
	const n = 50
	rs := newRaceServer(t)

	for round := range raceRounds {
		tenant := rs.activeTenant(t, fmt.Sprintf("race-patch-%d", round)) // version 2

		results := rs.race(t, n, func(i int) *http.Request {
			// Every request carries the same version and a different name.
			return rs.request(t, http.MethodPatch, "/v1/tenants/"+tenant.String(), fmt.Sprintf(`{"name":"Racer %d","version":2}`, i))
		})

		assertOutcomes(t, results, map[outcome]int{
			{http.StatusAccepted, ""}: 1,
			{http.StatusConflict, string(domain.CodeTenantVersionConflict)}: n - 1,
		})

		// Version +1 exactly once, and the winner's name is the one stored.
		var name string
		var version int
		var status string
		rs.queryRow(t, `SELECT name, version, status FROM tenants WHERE id = $1`, tenant).Scan(&name, &version, &status)
		if version != 3 || status != string(domain.TenantUpdating) {
			t.Errorf("round %d: version=%d status=%s, want 3/updating", round, version, status)
		}
		if winner := winnerBody(t, results); !strings.Contains(winner, fmt.Sprintf("%q", name)) {
			t.Errorf("round %d: stored name %q is not the winner's (%s)", round, name, winner)
		}
		rs.assertOneOpenTask(t, tenant, domain.TaskUpdate)
		rs.assertCount(t, 2, `SELECT count(*) FROM outbox o JOIN tasks t ON t.id = o.task_id WHERE t.tenant_id = $1`, tenant) // deploy + one update
	}
}

func TestRaceConcurrentDeletes(t *testing.T) {
	const n = 20
	rs := newRaceServer(t)

	for round := range raceRounds {
		tenant := rs.activeTenant(t, fmt.Sprintf("race-delete-%d", round)) // version 2

		results := rs.race(t, n, func(int) *http.Request {
			return rs.request(t, http.MethodDelete, "/v1/tenants/"+tenant.String(), "")
		})

		assertOutcomes(t, results, map[outcome]int{
			{http.StatusAccepted, ""}: 1,
			{http.StatusConflict, string(domain.CodeTenantUpdateNotAllowed)}: n - 1,
		})
		rs.assertCount(t, 1, `SELECT count(*) FROM tenants WHERE id = $1 AND version = 3 AND status = 'destroying'`, tenant)
		rs.assertOneOpenTask(t, tenant, domain.TaskDestroy)
	}
}

// Beyond the plan: PATCH and DELETE racing each other on the same tenant.
// Exactly one of all the requests wins, whichever kind it is. Losing PATCHes
// see the version the winner bumped; losing DELETEs see a tenant that is no
// longer active.
func TestRaceConcurrentPatchAndDelete(t *testing.T) {
	const n = 40
	rs := newRaceServer(t)

	for round := range raceRounds {
		tenant := rs.activeTenant(t, fmt.Sprintf("race-mixed-%d", round)) // version 2

		results := rs.race(t, n, func(i int) *http.Request {
			if i%2 == 0 {
				return rs.request(t, http.MethodPatch, "/v1/tenants/"+tenant.String(), `{"name":"Racer","version":2}`)
			}
			return rs.request(t, http.MethodDelete, "/v1/tenants/"+tenant.String(), "")
		})

		wins := 0
		for _, r := range results {
			switch {
			case r.status == http.StatusAccepted:
				wins++
			case r.status == http.StatusConflict && r.method == http.MethodPatch && r.code == string(domain.CodeTenantVersionConflict):
			case r.status == http.StatusConflict && r.method == http.MethodDelete && r.code == string(domain.CodeTenantUpdateNotAllowed):
			default:
				t.Errorf("round %d: unexpected %s outcome %d %s", round, r.method, r.status, r.code)
			}
		}
		if wins != 1 {
			t.Errorf("round %d: %d requests won, want exactly 1", round, wins)
		}
		rs.assertCount(t, 1, `SELECT count(*) FROM tenants WHERE id = $1 AND version = 3`, tenant)
		rs.assertCount(t, 1, `SELECT count(*) FROM tasks WHERE tenant_id = $1 AND status = 'accepted'`, tenant)
	}
}

// ---- race harness ----

type raceServer struct {
	srv    *httptest.Server
	pool   *pgxpool.Pool
	client *http.Client

	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

type outcome struct {
	status int
	code   string // the error code; "" for a success
}

type result struct {
	method string
	outcome
	body string
}

func newRaceServer(t *testing.T) *raceServer {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs Postgres (run make test)")
	}

	u, err := url.Parse(testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", fmt.Sprint(raceConns))
	u.RawQuery = q.Encode()

	pool, err := store.Open(context.Background(), u.String())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(pool.Close)

	rs := &raceServer{pool: pool}
	router := NewRouter(Deps{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), DB: pool, Store: store.New(pool)})
	// Count requests inside the handler at once: the proof that they overlapped.
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := rs.inFlight.Add(1)
		defer rs.inFlight.Add(-1)
		for {
			peak := rs.maxInFlight.Load()
			if now <= peak || rs.maxInFlight.CompareAndSwap(peak, now) {
				break
			}
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(rs.srv.Close)

	// One connection per racing request, so none waits for another's to free up.
	rs.client = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 100},
	}
	t.Cleanup(rs.client.CloseIdleConnections)
	return rs
}

// race sends n requests at once and returns their outcomes. Every goroutine
// builds its request, then waits at the barrier; closing start releases them
// together.
func (rs *raceServer) race(t *testing.T, n int, build func(i int) *http.Request) []result {
	t.Helper()
	rs.maxInFlight.Store(0)

	var ready, done sync.WaitGroup
	start := make(chan struct{})
	results := make([]result, n)

	for i := range n {
		req := build(i)
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			results[i] = rs.send(req)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()

	for _, r := range results {
		if r.status == 0 {
			t.Fatalf("a request failed at the transport level: %s", r.body)
		}
	}
	// With n requests released together, many must be in the handler at once;
	// if they ran one after another, this isn't a race test.
	if peak := rs.maxInFlight.Load(); peak < 2 {
		t.Fatalf("at most %d request(s) in flight at once: the requests did not overlap", peak)
	}
	t.Logf("%d requests, up to %d in flight at once → %s", n, rs.maxInFlight.Load(), tally(results))
	return results
}

func (rs *raceServer) send(req *http.Request) result {
	res := result{method: req.Method}
	resp, err := rs.client.Do(req)
	if err != nil {
		res.body = err.Error()
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	res.status = resp.StatusCode
	res.body = string(raw)
	if resp.StatusCode >= 400 {
		var body errorBody
		if json.Unmarshal(raw, &body) == nil {
			res.code = string(body.Error.Code)
		}
	}
	return res
}

func (rs *raceServer) request(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, rs.srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// activeTenant creates a tenant and completes its deploy, leaving it active at
// version 2.
func (rs *raceServer) activeTenant(t *testing.T, slug string) uuid.UUID {
	t.Helper()
	res := rs.send(rs.request(t, http.MethodPost, "/v1/tenants", fmt.Sprintf(`{"slug":%q,"name":"Racer"}`, slug)))
	if res.status != http.StatusCreated {
		t.Fatalf("create %s: %d %s", slug, res.status, res.body)
	}
	var body mutationView
	if err := json.Unmarshal([]byte(res.body), &body); err != nil {
		t.Fatal(err)
	}
	testutil.FinishTask(t, rs.pool, body.Tenant.ID, domain.TenantActive)
	return body.Tenant.ID
}

func (rs *raceServer) queryRow(t *testing.T, sql string, args ...any) interface{ Scan(...any) } {
	t.Helper()
	return scanOrFail{t: t, row: rs.pool.QueryRow(context.Background(), sql, args...)}
}

type scanOrFail struct {
	t   *testing.T
	row interface{ Scan(...any) error }
}

func (s scanOrFail) Scan(dest ...any) {
	s.t.Helper()
	if err := s.row.Scan(dest...); err != nil {
		s.t.Fatalf("query: %v", err)
	}
}

func (rs *raceServer) assertCount(t *testing.T, want int, sql string, args ...any) {
	t.Helper()
	var got int
	rs.queryRow(t, sql, args...).Scan(&got)
	if got != want {
		t.Errorf("%s = %d, want %d", sql, got, want)
	}
}

// assertOneOpenTask checks the "at most one open task per tenant" rule held,
// and that the open task is the winner's.
func (rs *raceServer) assertOneOpenTask(t *testing.T, tenant uuid.UUID, typ domain.TaskType) {
	t.Helper()
	rs.assertCount(t, 1, `SELECT count(*) FROM tasks WHERE tenant_id = $1 AND status IN ('accepted', 'in_progress')`, tenant)
	rs.assertCount(t, 1, `SELECT count(*) FROM tasks WHERE tenant_id = $1 AND status = 'accepted' AND type = $2`, tenant, string(typ))
}

func assertOutcomes(t *testing.T, results []result, want map[outcome]int) {
	t.Helper()
	got := map[outcome]int{}
	for _, r := range results {
		got[r.outcome]++
	}
	if len(got) != len(want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for o, n := range want {
		if got[o] != n {
			t.Fatalf("outcomes = %v, want %v", got, want)
		}
	}
}

// tally renders outcome counts for the log, e.g. "1 × 202, 49 × 409 tenant_version_conflict".
func tally(results []result) string {
	counts := map[string]int{}
	for _, r := range results {
		key := fmt.Sprint(r.status)
		if r.code != "" {
			key += " " + r.code
		}
		counts[key]++
	}
	keys := slices.Sorted(maps.Keys(counts))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d × %s", counts[k], k)
	}
	return strings.Join(parts, ", ")
}

func winnerBody(t *testing.T, results []result) string {
	t.Helper()
	for _, r := range results {
		if r.status < 300 {
			return r.body
		}
	}
	t.Fatal("no winning request")
	return ""
}
