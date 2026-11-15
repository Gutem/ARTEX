package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func httpToolSet(t *testing.T) *ToolSet {
	t.Helper()
	return NewToolSet(nil, "worker")
}

func callHTTP(t *testing.T, ts *ToolSet, name, args string) string {
	t.Helper()
	for _, tl := range ts.HTTPTools() {
		if tl.Name() != name {
			continue
		}
		res, err := tl.Call(context.Background(), json.RawMessage(args), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s returned a tool error: %s", name, res.Flatten())
		}
		return res.Flatten()
	}
	t.Fatalf("%s not found in the tool set", name)
	return ""
}

func TestHTTPRequestReturnsStructure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello from the server"))
	}))
	defer srv.Close()

	ts := httpToolSet(t)
	out := callHTTP(t, ts, "http_request", `{"url":"`+srv.URL+`"}`)
	if !strings.Contains(out, "→ 418") {
		t.Fatalf("status missing from the structured result: %s", out)
	}
	if !strings.Contains(out, "hello from the server") {
		t.Fatalf("body snippet missing: %s", out)
	}
	if !strings.Contains(out, "X-Test: yes") {
		t.Fatalf("relevant header missing: %s", out)
	}
}

func TestHTTPRequestSharesCookieJar(t *testing.T) {
	var sawCookie string
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc123", Path: "/"})
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/private", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("sid"); err == nil {
			sawCookie = c.Value
		}
		_, _ = w.Write([]byte("private body"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := httpToolSet(t)
	login := callHTTP(t, ts, "session_login", `{"url":"`+srv.URL+`/login","username":"u","password":"p","session":"alice"}`)
	if !strings.Contains(login, "1 cookie(s) stored") {
		t.Fatalf("login should report the stored cookie: %s", login)
	}
	callHTTP(t, ts, "http_request", `{"url":"`+srv.URL+`/private","session":"alice"}`)
	if sawCookie != "abc123" {
		t.Fatalf("the session cookie was not sent on the follow-up request (saw %q)", sawCookie)
	}
}

func TestHTTPRequestSessionsAreIsolated(t *testing.T) {
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		user := r.FormValue("username")
		http.SetCookie(w, &http.Cookie{Name: "user", Value: user, Path: "/"})
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("user"); err == nil {
			seen = append(seen, c.Value)
		}
		_, _ = w.Write([]byte("body"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := httpToolSet(t)
	callHTTP(t, ts, "session_login", `{"url":"`+srv.URL+`/login","username":"alice","password":"p","session":"alice"}`)
	callHTTP(t, ts, "session_login", `{"url":"`+srv.URL+`/login","username":"bob","password":"p","session":"bob"}`)
	callHTTP(t, ts, "http_request", `{"url":"`+srv.URL+`/whoami","session":"alice"}`)
	callHTTP(t, ts, "http_request", `{"url":"`+srv.URL+`/whoami","session":"bob"}`)
	if len(seen) != 2 || seen[0] != "alice" || seen[1] != "bob" {
		t.Fatalf("sessions leaked into each other: %v", seen)
	}
}

func TestHTTPDiffReportsIdenticalBodiesPlainly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"order":42,"owner":"alice"}`))
	}))
	defer srv.Close()

	ts := httpToolSet(t)
	out := callHTTP(t, ts, "http_diff", `{"a":{"url":"`+srv.URL+`/o/42","session":"alice"},"b":{"url":"`+srv.URL+`/o/42","session":"bob"}}`)
	if !strings.Contains(out, "body: IDENTICAL") {
		t.Fatalf("identical bodies must be reported as identical: %s", out)
	}
	if !strings.Contains(out, "do not claim one") {
		t.Fatalf("the output should warn against inventing a difference: %s", out)
	}
}

func TestHTTPDiffShowsStatusAndBodyChange(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/alice", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"order":42,"owner":"alice","total":"R$ 10"}`))
	})
	mux.HandleFunc("/bob", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := httpToolSet(t)
	out := callHTTP(t, ts, "http_diff", `{"a":{"url":"`+srv.URL+`/alice"},"b":{"url":"`+srv.URL+`/bob"}}`)
	if !strings.Contains(out, "status differs: 200 vs 403") {
		t.Fatalf("status difference not reported: %s", out)
	}
	if !strings.Contains(out, "body: differs") {
		t.Fatalf("body difference not reported: %s", out)
	}
	if !strings.Contains(out, "B 1:") {
		t.Fatalf("the first differing line of B is missing: %s", out)
	}
}

func TestHTTPRequestRejectsBadURL(t *testing.T) {
	ts := httpToolSet(t)
	for _, tl := range ts.HTTPTools() {
		if tl.Name() != "http_request" {
			continue
		}
		res, err := tl.Call(context.Background(), json.RawMessage(`{"url":"not a url"}`), nil)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !res.IsError {
			t.Fatalf("an invalid URL must be a tool error, got %s", res.Flatten())
		}
	}
}
