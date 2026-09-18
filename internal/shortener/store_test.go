package shortener

import (
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("creating schema: %v", err)
	}
	return store
}

func TestNormaliseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.com/a", "https://example.com/a"},
		{"example.com", "https://example.com"},
		{"  http://EXAMPLE.com/Path?q=1  ", "http://example.com/Path?q=1"},
	}
	for _, c := range cases {
		got, err := NormaliseURL(c.in)
		if err != nil || got != c.want {
			t.Errorf("NormaliseURL(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "   ", "javascript:alert(1)", "ftp://files.example.com", "not a url", "http://localhost"} {
		if _, err := NormaliseURL(bad); err == nil {
			t.Errorf("NormaliseURL(%q) should have failed", bad)
		}
	}
}

func TestCodeAlphabetAvoidsLookAlikes(t *testing.T) {
	for _, char := range "01lIO" {
		if strings.ContainsRune(alphabet, char) {
			t.Errorf("alphabet should not contain %q: it is easy to misread", char)
		}
	}
	code, err := NewCode(8)
	if err != nil || len(code) != 8 {
		t.Fatalf("NewCode(8) = %q, %v", code, err)
	}
}

func TestCreateAndResolve(t *testing.T) {
	store := newTestStore(t)
	link, err := store.Create("https://example.com/docs", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(link.Code) < 6 {
		t.Errorf("generated code %q is too short", link.Code)
	}

	resolved, err := store.Resolve(link.Code)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Target != "https://example.com/docs" {
		t.Errorf("target = %q", resolved.Target)
	}
	if resolved.Visits != 1 {
		t.Errorf("visits = %d, want 1", resolved.Visits)
	}
	if again, _ := store.Get(link.Code); again.Visits != 1 {
		t.Errorf("Get should not count a visit, visits = %d", again.Visits)
	}
}

func TestCustomCodes(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Create("https://example.com", "my-link", 0); err != nil {
		t.Fatalf("custom code: %v", err)
	}
	if _, err := store.Create("https://other.example", "my-link", 0); err != ErrCodeTaken {
		t.Errorf("duplicate code error = %v, want ErrCodeTaken", err)
	}
	for _, bad := range []string{"ab", "has space", "way-too-long-" + strings.Repeat("x", 40), "api", "health"} {
		if _, err := store.Create("https://example.com", bad, 0); err != ErrBadCode {
			t.Errorf("Create(code=%q) error = %v, want ErrBadCode", bad, err)
		}
	}
}

func TestExpiry(t *testing.T) {
	store := newTestStore(t)
	link, err := store.Create("https://example.com", "soon", time.Millisecond)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.ExpiresAt == nil {
		t.Fatal("ExpiresAt should be set")
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := store.Resolve("soon"); err != ErrExpired {
		t.Errorf("Resolve after expiry = %v, want ErrExpired", err)
	}
	purged, err := store.PurgeExpired()
	if err != nil || purged != 1 {
		t.Errorf("PurgeExpired = %d, %v; want 1", purged, err)
	}
	if _, err := store.Get("soon"); err != ErrNotFound {
		t.Errorf("after purge Get = %v, want ErrNotFound", err)
	}
}

func TestListDeleteAndStats(t *testing.T) {
	store := newTestStore(t)
	for _, target := range []string{"https://a.example", "https://b.example", "https://c.example"} {
		if _, err := store.Create(target, "", 0); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	links, err := store.List(2)
	if err != nil || len(links) != 2 {
		t.Fatalf("List(2) = %d links, %v", len(links), err)
	}
	stats, _ := store.Stats()
	if stats["links"].(int64) != 3 {
		t.Errorf("stats links = %v, want 3", stats["links"])
	}
	if err := store.Delete(links[0].Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete("nope"); err != ErrNotFound {
		t.Errorf("deleting a missing code = %v, want ErrNotFound", err)
	}
}

func TestConcurrentVisitsAreAllCounted(t *testing.T) {
	store := newTestStore(t)
	link, err := store.Create("https://example.com", "busy", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const hits = 50
	var wg sync.WaitGroup
	wg.Add(hits)
	for i := 0; i < hits; i++ {
		go func() {
			defer wg.Done()
			if _, err := store.Resolve(link.Code); err != nil {
				t.Errorf("Resolve: %v", err)
			}
		}()
	}
	wg.Wait()
	final, _ := store.Get(link.Code)
	if final.Visits != hits {
		t.Errorf("visits = %d, want %d: the counter must not lose concurrent hits", final.Visits, hits)
	}
}

func TestConcurrentCreatesGetUniqueCodes(t *testing.T) {
	store := newTestStore(t)
	const n = 40
	var wg sync.WaitGroup
	codes := make(chan string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			link, err := store.Create("https://example.com", "", 0)
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			codes <- link.Code
		}()
	}
	wg.Wait()
	close(codes)
	seen := map[string]bool{}
	for code := range codes {
		if seen[code] {
			t.Fatalf("code %q was handed out twice", code)
		}
		seen[code] = true
	}
	if len(seen) != n {
		t.Errorf("got %d codes, want %d", len(seen), n)
	}
}
