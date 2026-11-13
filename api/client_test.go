package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testCookie = "p_session_token"

func tokenFrom(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Cookie"), testCookie+"=")
}

func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

// Finding #10: a request must use one consistent (baseURL, token) snapshot,
// and the setters must be race-free (run with -race).
func TestSnapshotConsistencyUnderConcurrentSetSession(t *testing.T) {
	var mismatches, hitsA, hitsB atomic.Int64
	mk := func(want string, hits *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if tokenFrom(r) != want {
				mismatches.Add(1)
			}
			writeOK(w, map[string]any{})
		}))
	}
	a := mk("tokA", &hitsA)
	defer a.Close()
	b := mk("tokB", &hitsB)
	defer b.Close()

	c := NewAPIClient(a.URL, "tokA")
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				c.SetSession(a.URL, "tokA")
			} else {
				c.SetSession(b.URL, "tokB")
			}
		}
	}()

	var workers sync.WaitGroup
	for w := 0; w < 4; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 60; i++ {
				if _, _, err := c.makeRequest("GET", "/ping", nil); err != nil {
					t.Errorf("request failed: %v", err)
					return
				}
				_ = c.CurrentBaseURL()
				_ = c.SessionToken()
			}
		}()
	}
	workers.Wait()
	close(stop)
	wg.Wait()

	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d requests reached a server with the other server's token", n)
	}
	if hitsA.Load() == 0 || hitsB.Load() == 0 {
		t.Logf("note: hits A=%d B=%d (one server unused, weak interleaving)", hitsA.Load(), hitsB.Load())
	}
}

// Finding #10: the individual setters race with requests without data races.
func TestIndividualSettersAreRaceFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeOK(w, map[string]any{}) }))
	defer srv.Close()
	c := NewAPIClient(srv.URL, "t")
	c.SetOnUnauthorized(func(string) {})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range []func(){
		func() { c.UpdateBaseURL(srv.URL) },
		func() { c.UpdateSessionToken("x") },
		func() { c.SetSessionCookieName(testCookie) },
		func() { c.SetOnUnauthorized(func(string) {}) },
	} {
		wg.Add(1)
		go func(f func()) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					f()
				}
			}
		}(f)
	}
	for i := 0; i < 40; i++ {
		if _, _, err := c.makeRequest("GET", "/ping", nil); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// Finding #10: 401 only reports the session expired if the client still holds
// the token the request was sent with.
func TestOnUnauthorizedOnlyForCurrentToken(t *testing.T) {
	cases := []struct {
		name       string
		switchTok  bool
		switchBase bool
		want       int32
	}{
		{"same token fires", false, false, 1},
		{"token switched while in flight: no callback", true, false, 0},
		{"base URL switched while in flight: no callback", false, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arrived := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(arrived)
				<-release
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer srv.Close()

			c := NewAPIClient(srv.URL, "old")
			var calls atomic.Int32
			c.SetOnUnauthorized(func(string) { calls.Add(1) })

			done := make(chan error, 1)
			go func() {
				_, resp, err := c.makeRequest("GET", "/x", nil)
				if err == nil && resp.StatusCode != 401 {
					err = fmt.Errorf("status %d", resp.StatusCode)
				}
				done <- err
			}()
			<-arrived
			if tc.switchTok {
				c.UpdateSessionToken("new")
			}
			if tc.switchBase {
				c.UpdateBaseURL("http://127.0.0.1:1")
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request did not finish")
			}
			if got := calls.Load(); got != tc.want {
				t.Fatalf("callback calls = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestOnUnauthorizedNotFiredWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	c := NewAPIClient(srv.URL, "")
	var calls atomic.Int32
	c.SetOnUnauthorized(func(string) { calls.Add(1) })
	if _, _, err := c.makeRequest("GET", "/x", nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("callback fired for a request sent without a token")
	}
}

// Finding #10: Logout() answering 401 only means the session was already gone.
func TestLogoutNeverFiresOnUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/logout" || r.Method != "POST" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"nope"}`)
	}))
	defer srv.Close()
	c := NewAPIClient(srv.URL, "tok")
	var calls atomic.Int32
	c.SetOnUnauthorized(func(string) { calls.Add(1) })
	err := c.Logout()
	if HTTPStatus(err) != 401 {
		t.Fatalf("Logout err = %v, want a 401 APIError", err)
	}
	if calls.Load() != 0 {
		t.Fatal("Logout fired onUnauthorized")
	}
}

// Finding #12: context-aware calls abort promptly and wrap context.Canceled.
func TestContextCancellationAborts(t *testing.T) {
	calls := map[string]func(c *APIClient, ctx context.Context) error{
		"makeRequestContext": func(c *APIClient, ctx context.Context) error {
			_, _, err := c.makeRequestContext(ctx, "GET", "/x", nil)
			return err
		},
		"StartDeviceAuthContext": func(c *APIClient, ctx context.Context) error {
			_, err := c.StartDeviceAuthContext(ctx, "app", nil)
			return err
		},
		"PollDeviceAuthContext": func(c *APIClient, ctx context.Context) error {
			_, _, err := c.PollDeviceAuthContext(ctx, "CODE")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			arrived := make(chan struct{}, 1)
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case arrived <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer srv.Close()
			defer close(release)

			c := NewAPIClient(srv.URL, "tok")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- call(c, ctx) }()
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("request never reached server")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want errors.Is context.Canceled", err)
				}
				if IsSessionRejected(err) {
					t.Fatal("cancellation classified as session rejection")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("call did not abort promptly after cancel")
			}
		})
	}
}

func TestContextAlreadyCancelled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := NewAPIClient(srv.URL, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.StartDeviceAuthContext(ctx, "app", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("request sent despite cancelled context")
	}
}

// Finding #13: gateway listing pagination.
func gwRows(from, to int, mode func(i int) string) []SiteResource {
	var rows []SiteResource
	for i := from; i < to; i++ {
		rows = append(rows, SiteResource{SiteResourceID: i, Name: fmt.Sprint("r", i), Mode: mode(i), SiteIDs: []int{1}})
	}
	return rows
}

func allGateway(int) string { return "gateway" }

func TestListGatewayResourcesPrePaginationServerTerminates(t *testing.T) {
	// Ignores page/pageSize; always returns the same 120 rows, every 3rd is not a gateway.
	rows := gwRows(1, 121, func(i int) string {
		if i%3 == 0 {
			return "host"
		}
		return "gateway"
	})
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		if reqs.Load() > 50 {
			http.Error(w, "runaway", 500)
			return
		}
		writeOK(w, map[string]any{"siteResources": rows})
	}))
	defer srv.Close()

	done := make(chan struct{})
	var got []SiteResource
	var err error
	go func() {
		defer close(done)
		got, err = NewAPIClient(srv.URL, "t").ListGatewayResources("org")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ListGatewayResources did not terminate")
	}
	if err != nil {
		t.Fatal(err)
	}
	if n := reqs.Load(); n > 3 {
		t.Fatalf("made %d requests against a non-paginating server", n)
	}
	seen := map[int]int{}
	for _, r := range got {
		seen[r.SiteResourceID]++
		if r.Mode != "gateway" {
			t.Fatalf("non-gateway row %d returned", r.SiteResourceID)
		}
	}
	if len(got) != 80 {
		t.Fatalf("got %d gateways, want 80", len(got))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("row %d returned %d times", id, n)
		}
	}
}

func TestListGatewayResourcesPaginatingServer(t *testing.T) {
	all := gwRows(1, 251, allGateway)
	var pages []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("mode") != "gateway" {
			t.Errorf("mode = %q", q.Get("mode"))
		}
		var page, size int
		fmt.Sscan(q.Get("page"), &page)
		fmt.Sscan(q.Get("pageSize"), &size)
		mu.Lock()
		pages = append(pages, q.Get("page"))
		mu.Unlock()
		lo, hi := (page-1)*size, page*size
		if lo > len(all) {
			lo = len(all)
		}
		if hi > len(all) {
			hi = len(all)
		}
		writeOK(w, map[string]any{"siteResources": all[lo:hi]})
	}))
	defer srv.Close()

	got, err := NewAPIClient(srv.URL, "t").ListGatewayResources("org")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 250 {
		t.Fatalf("got %d rows, want 250", len(got))
	}
	if len(pages) != 3 {
		t.Fatalf("pages requested = %v, want 3", pages)
	}
}

func TestListGatewayResourcesStopsAtEmptyPage(t *testing.T) {
	all := gwRows(1, 101, allGateway) // exactly one full page, then empty
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqs.Add(1)
		if n > 10 {
			http.Error(w, "runaway", 500)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			writeOK(w, map[string]any{"siteResources": all})
			return
		}
		writeOK(w, map[string]any{"siteResources": []SiteResource{}})
	}))
	defer srv.Close()
	got, err := NewAPIClient(srv.URL, "t").ListGatewayResources("org")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 || reqs.Load() != 2 {
		t.Fatalf("rows=%d requests=%d, want 100 rows in 2 requests", len(got), reqs.Load())
	}
}

// Finding #101: redirects must not turn a POST into a GET.
func TestRedirectPostHttpToHttpsFails(t *testing.T) {
	for _, code := range []int{301, 302, 303} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var targetHits atomic.Int32
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetHits.Add(1)
				t.Errorf("target saw %s", r.Method)
			}))
			defer target.Close()
			var origin *httptest.Server
			origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, code)
			}))
			defer origin.Close()

			c := NewAPIClient(origin.URL, "")
			_, _, err := c.makeRequest("POST", "/auth/login", []byte(`{"a":1}`))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "https") {
				t.Fatalf("error %q does not mention https", err)
			}
			if targetHits.Load() != 0 {
				t.Fatal("redirect target was contacted")
			}
		})
	}
}

func TestRedirectPostSameSchemeDowngradeRejected(t *testing.T) {
	var getHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			getHits.Add(1)
		}
		writeOK(w, map[string]any{})
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, 302)
	}))
	defer origin.Close()
	_, _, err := NewAPIClient(origin.URL, "").makeRequest("POST", "/x", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for POST->GET redirect")
	}
	if getHits.Load() != 0 {
		t.Fatal("POST was re-sent as GET")
	}
}

func TestRedirect307And308PreservePostAndBody(t *testing.T) {
	for _, code := range []int{307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			type seen struct{ method, body string }
			ch := make(chan seen, 1)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				ch <- seen{r.Method, string(b)}
				writeOK(w, map[string]any{})
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, code)
			}))
			defer origin.Close()
			_, resp, err := NewAPIClient(origin.URL, "").makeRequest("POST", "/x", []byte(`{"k":"v"}`))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			s := <-ch
			if s.method != "POST" || s.body != `{"k":"v"}` {
				t.Fatalf("target saw %s %q", s.method, s.body)
			}
		})
	}
}

func TestRedirectGetStillWorks(t *testing.T) {
	for _, code := range []int{301, 302, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("method %s", r.Method)
				}
				writeOK(w, map[string]any{})
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, code)
			}))
			defer origin.Close()
			_, resp, err := NewAPIClient(origin.URL, "").makeRequest("GET", "/x", nil)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("err=%v resp=%v", err, resp)
			}
		})
	}
}

func TestRedirectLimit(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.Path, 302)
	}))
	defer srv.Close()
	if _, _, err := NewAPIClient(srv.URL, "").makeRequest("GET", "/x", nil); err == nil {
		t.Fatal("expected redirect loop error")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestErrorClassification(t *testing.T) {
	httpErr := func(s int) error { return &APIError{Type: ErrorTypeHTTPError, Status: s, Message: "m"} }
	tests := []struct {
		name      string
		err       error
		status    int
		rejected  bool
		transient bool
	}{
		{"nil", nil, 0, false, false},
		{"network error", &APIError{Type: ErrorTypeNetworkError, Err: errors.New("refused")}, 0, false, true},
		{"timeout", &APIError{Type: ErrorTypeHTTPError, Message: "timed out", Err: timeoutErr{}}, 0, false, true},
		{"plain error", errors.New("boom"), 0, false, true},
		{"cancelled", &APIError{Type: ErrorTypeNetworkError, Err: context.Canceled}, 0, false, true},
		{"decode error", &APIError{Type: ErrorTypeDecodingError, Err: errors.New("bad json")}, 0, false, true},
		{"invalid response", &APIError{Type: ErrorTypeInvalidResponse, Message: "Empty response"}, 0, false, true},
		{"429", httpErr(429), 429, false, true},
		{"500", httpErr(500), 500, false, true},
		{"503", httpErr(503), 503, false, true},
		{"401", httpErr(401), 401, true, false},
		{"403", httpErr(403), 403, true, false},
		{"404", httpErr(404), 404, false, false},
		{"400", httpErr(400), 400, false, false},
		{"wrapped 401", fmt.Errorf("ctx: %w", httpErr(401)), 401, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HTTPStatus(tc.err); got != tc.status {
				t.Errorf("HTTPStatus = %d, want %d", got, tc.status)
			}
			if got := IsSessionRejected(tc.err); got != tc.rejected {
				t.Errorf("IsSessionRejected = %v, want %v", got, tc.rejected)
			}
			if got := IsTransient(tc.err); got != tc.transient {
				t.Errorf("IsTransient = %v, want %v", got, tc.transient)
			}
		})
	}
}

// End to end: real responses classify correctly.
func TestErrorClassificationFromServer(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		c := NewAPIClient(srv.URL, "")
		_, err := c.GetUser()
		srv.Close()
		if HTTPStatus(err) != status {
			t.Errorf("status %d: HTTPStatus = %d (err %v)", status, HTTPStatus(err), err)
		}
	}
	// Connection refused is transient.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := NewAPIClient(url, "").GetUser(); !IsTransient(err) {
		t.Errorf("connection refused not transient: %v", err)
	}
}

// The 401 callback gets the token the rejected request was sent with, so the
// receiver can check it is still the active session's.
func TestOnUnauthorizedReceivesRequestToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	c := NewAPIClient(srv.URL, "tok-1")
	var got string
	c.SetOnUnauthorized(func(token string) { got = token })
	if _, _, err := c.makeRequest("GET", "/x", nil); err != nil {
		t.Fatal(err)
	}
	if got != "tok-1" {
		t.Fatalf("callback got token %q, want %q", got, "tok-1")
	}
}
