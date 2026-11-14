package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestForbiddenClassification(t *testing.T) {
	httpErr := func(status int, msg string) error {
		return &APIError{Type: ErrorTypeHTTPError, Status: status, Message: msg}
	}
	tests := []struct {
		name      string
		err       error
		status    int
		forbidden bool
	}{
		{"nil", nil, 0, false},
		{"network", &APIError{Type: ErrorTypeNetworkError, Err: errors.New("dial")}, 0, false},
		{"plain error", errors.New("x"), 0, false},
		{"403", httpErr(403, "User does not have permission"), 403, true},
		{"403 wrapped", fmt.Errorf("wrap: %w", httpErr(403, "")), 403, true},
		{"404", httpErr(404, "Not found"), 404, false},
		{"500", httpErr(500, "boom"), 500, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HTTPStatus(tt.err); got != tt.status {
				t.Errorf("HTTPStatus = %d, want %d", got, tt.status)
			}
			if got := IsForbidden(tt.err); got != tt.forbidden {
				t.Errorf("IsForbidden = %v, want %v", got, tt.forbidden)
			}
		})
	}
}

// writeAPI writes a Pangolin-style response envelope.
func writeAPI(w http.ResponseWriter, status int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	ok := status < 300
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data, "success": ok, "error": !ok, "message": message, "status": status,
	})
}

func TestUpdateClientNameRequestAndErrors(t *testing.T) {
	var gotMethod, gotPath, gotCookie string
	var gotBody map[string]any
	status, message := http.StatusOK, "ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCookie = r.Method, r.URL.Path, r.Header.Get("Cookie")
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if status != http.StatusOK {
			writeAPI(w, status, message, nil)
			return
		}
		writeAPI(w, status, message, map[string]any{"clientId": 7, "name": gotBody["name"]})
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	if err := c.UpdateClientName(context.Background(), 7, "my-host"); err != nil {
		t.Fatalf("UpdateClientName: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/api/v1/client/7" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotCookie != "p_session_token=tok" {
		t.Errorf("cookie = %q", gotCookie)
	}
	if len(gotBody) != 1 || gotBody["name"] != "my-host" {
		t.Errorf("body = %v, want only the name", gotBody)
	}

	status, message = http.StatusForbidden, "User does not have permission to perform this action"
	if err := c.UpdateClientName(context.Background(), 7, "x"); !IsForbidden(err) {
		t.Errorf("403: IsForbidden(%v) = false", err)
	}
}

// strictUserDevices imitates GET /org/:orgId/user-devices: before Pangolin
// 1.26.0 the query schema is a strict object without userId, so the filter is
// answered with 400.
func strictUserDevices(t *testing.T, knowsUserID bool, devices []UserDevice, queries *[]string) http.Handler {
	t.Helper()
	allowed := map[string]bool{"pageSize": true, "page": true, "query": true, "sort_by": true,
		"order": true, "online": true, "agent": true, "status": true}
	if knowsUserID {
		allowed["userId"] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*queries = append(*queries, r.URL.RawQuery)
		q := r.URL.Query()
		for k := range q {
			if !allowed[k] {
				writeAPI(w, http.StatusBadRequest, fmt.Sprintf("Validation error: Unrecognized key: %q", k), nil)
				return
			}
		}
		out := []UserDevice{}
		for _, d := range devices {
			if uid := q.Get("userId"); uid == "" || (d.UserId != nil && *d.UserId == uid) {
				out = append(out, d)
			}
		}
		writeAPI(w, http.StatusOK, "ok", map[string]any{"devices": out})
	})
}

func TestListUserDevicesOnOlderServers(t *testing.T) {
	u1, u2 := "u1", "u2"
	devices := []UserDevice{
		{ClientId: 1, OrgId: "o", Name: "Linux Laptop", UserId: &u1},
		{ClientId: 2, OrgId: "o", Name: "Linux Laptop", UserId: &u2},
		{ClientId: 3, OrgId: "o", Name: "machine"},
	}
	for _, knowsUserID := range []bool{true, false} {
		var queries []string
		srv := httptest.NewServer(strictUserDevices(t, knowsUserID, devices, &queries))
		got, err := NewAPIClient(srv.URL, "tok").ListUserDevices(context.Background(), "o", "u1")
		srv.Close()
		if err != nil {
			t.Fatalf("knowsUserID=%v: %v", knowsUserID, err)
		}
		if len(got) != 1 || got[0].ClientId != 1 {
			t.Errorf("knowsUserID=%v: got %+v, want only u1's client 1", knowsUserID, got)
		}
		wantCalls := 1
		if !knowsUserID {
			wantCalls = 2 // the filtered call is refused, then repeated without it
		}
		if len(queries) != wantCalls {
			t.Errorf("knowsUserID=%v: %d calls %v, want %d", knowsUserID, len(queries), queries, wantCalls)
		}
	}
}

func TestRequestsHonorContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := NewAPIClient(srv.URL, "tok").UpdateClientName(ctx, 7, "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("request ignored the context for %v", time.Since(start))
	}
}
