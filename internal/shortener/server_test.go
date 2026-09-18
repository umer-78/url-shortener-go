package shortener

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const testPage = `<!doctype html><title>t</title>{{range .Links}}<a href="/{{.code}}">{{.code}}</a>{{end}}`

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(store, "https://s.example", testPage)
}

func post(t *testing.T, server *Server, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(body))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	var parsed map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &parsed)
	return recorder, parsed
}

func TestCreateEndpoint(t *testing.T) {
	server := newTestServer(t)
	recorder, body := post(t, server, `{"url":"https://example.com/page"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
	}
	code, _ := body["code"].(string)
	if want := "https://s.example/" + code; body["short_url"] != want {
		t.Errorf("short_url = %v, want %v", body["short_url"], want)
	}
}

func TestCreateValidation(t *testing.T) {
	server := newTestServer(t)
	cases := []struct {
		body string
		want int
	}{
		{`{"url":"javascript:alert(1)"}`, http.StatusUnprocessableEntity},
		{`{"url":""}`, http.StatusUnprocessableEntity},
		{`{"url":"https://example.com","code":"ab"}`, http.StatusUnprocessableEntity},
		{`{"url":"https://example.com","expires_in":"soon"}`, http.StatusUnprocessableEntity},
		{`not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		recorder, _ := post(t, server, c.body)
		if recorder.Code != c.want {
			t.Errorf("POST %s = %d, want %d", c.body, recorder.Code, c.want)
		}
	}
	post(t, server, `{"url":"https://example.com","code":"taken"}`)
	recorder, _ := post(t, server, `{"url":"https://other.example","code":"taken"}`)
	if recorder.Code != http.StatusConflict {
		t.Errorf("duplicate code = %d, want 409", recorder.Code)
	}
}

func TestRedirect(t *testing.T) {
	server := newTestServer(t)
	if recorder, _ := post(t, server, `{"url":"https://example.com/target","code":"docs"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("setup failed: %d %s", recorder.Code, recorder.Body)
	}

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if recorder.Code != http.StatusFound {
		t.Errorf("status = %d, want 302 (301 would be cached and stop counting visits)", recorder.Code)
	}
	if location := recorder.Header().Get("location"); location != "https://example.com/target" {
		t.Errorf("location = %q", location)
	}

	missing := httptest.NewRecorder()
	server.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/nothing-here", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("unknown code = %d, want 404", missing.Code)
	}
}

func TestListDeleteStatsAndPage(t *testing.T) {
	server := newTestServer(t)
	_, created := post(t, server, `{"url":"https://example.com"}`)
	code := created["code"].(string)

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/links", nil))
	var links []map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &links)
	if len(links) != 1 {
		t.Fatalf("list returned %d links", len(links))
	}

	page := httptest.NewRecorder()
	server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(page.Body.String(), code) {
		t.Errorf("the page should list the new code")
	}

	deleted := httptest.NewRecorder()
	server.ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/links/"+code, nil))
	if deleted.Code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", deleted.Code)
	}
	gone := httptest.NewRecorder()
	server.ServeHTTP(gone, httptest.NewRequest(http.MethodGet, "/api/links/"+code, nil))
	if gone.Code != http.StatusNotFound {
		t.Errorf("after delete = %d, want 404", gone.Code)
	}

	health := httptest.NewRecorder()
	server.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Errorf("health = %d", health.Code)
	}
}

func TestRateLimit(t *testing.T) {
	server := newTestServer(t)
	limited := false
	for i := 0; i < 40; i++ {
		recorder, _ := post(t, server, `{"url":"https://example.com"}`)
		if recorder.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("the rate limiter should stop a client after 30 links a minute")
	}
}
