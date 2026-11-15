package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// Native HTTP execution.
//
// Why it exists: driving every probe through Bash costs two round trips (tool
// call → shell → output), and the shell has no session. A login-then-IDOR chain
// then depends on the model carrying cookies by hand, and one mistake kills the
// direction. Running the request inside the process keeps the same egress path
// (the recording proxy), gives each task a cookie jar, and returns a structured
// result instead of a screenful of text.

// HTTPTools returns the native HTTP tools (http_request, session_login,
// http_diff). They are always offered to a worker: shell-only probing costs two
// round trips and has no session.
func (t *ToolSet) HTTPTools() []actool.CoreTool {
	return []actool.CoreTool{t.httpRequestTool(), t.sessionLoginTool(), t.httpDiffTool()}
}

// httpSession is one cookie jar plus the requests it produced, so a later
// http_diff can refer to a response by id without re-sending it.
type httpSession struct {
	jar  http.CookieJar
	mu   sync.Mutex
	last map[string]*httpResponse
}

type httpResponse struct {
	ID      string
	URL     string
	Method  string
	Status  int
	Headers http.Header
	Body    []byte
	Elapsed time.Duration
}

const (
	httpDefaultTimeout = 30 * time.Second
	httpDefaultBodyCap = 20000
	httpCachedBodies   = 20
)

// httpSessionsFor returns (creating on demand) the session with the given name.
// An empty name means the task's default session, so a plain http_request still
// reuses cookies — the common case of logging in once and probing after.
func (t *ToolSet) httpSessionsFor(name string) *httpSession {
	if name == "" {
		name = "default"
	}
	t.httpMu.Lock()
	defer t.httpMu.Unlock()
	if t.httpSessions == nil {
		t.httpSessions = map[string]*httpSession{}
	}
	s := t.httpSessions[name]
	if s == nil {
		jar, _ := cookiejar.New(nil)
		s = &httpSession{jar: jar, last: map[string]*httpResponse{}}
		t.httpSessions[name] = s
	}
	return s
}

// httpClient builds a client bound to the session jar, routed through the same
// egress proxy the shell uses. When a recording CA is configured, it is added to
// the pool so the MITM certificate validates instead of being blindly skipped.
func (t *ToolSet) httpClient(s *httpSession, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 || timeout > 5*time.Minute {
		timeout = httpDefaultTimeout
	}
	tr := &http.Transport{
		Proxy:                 nil,
		MaxIdleConnsPerHost:   4,
		ResponseHeaderTimeout: timeout,
	}
	if t.httpProxy != nil {
		if addr := strings.TrimSpace(t.httpProxy()); addr != "" {
			if pu, err := url.Parse(addr); err == nil {
				tr.Proxy = http.ProxyURL(pu)
			} else if pu, err := url.Parse("http://" + addr); err == nil {
				tr.Proxy = http.ProxyURL(pu)
			}
		}
	}
	if t.httpCACert != "" {
		if pem, err := os.ReadFile(t.httpCACert); err == nil {
			pool, _ := x509.SystemCertPool()
			if pool == nil {
				pool = x509.NewCertPool()
			}
			if pool.AppendCertsFromPEM(pem) {
				tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			}
		}
	}
	return &http.Client{Transport: tr, Jar: s.jar, Timeout: timeout, CheckRedirect: nil}, nil
}

// doHTTP performs one request and stores the response in the session cache.
func (t *ToolSet) doHTTP(ctx context.Context, session, id, method, rawURL string, headers map[string]string, body string, follow bool, timeout time.Duration, bodyCap int) (*httpResponse, error) {
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if rawURL == "" {
		return nil, fmt.Errorf("url is required")
	}
	if _, err := url.ParseRequestURI(rawURL); err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	s := t.httpSessionsFor(session)
	client, err := t.httpClient(s, timeout)
	if err != nil {
		return nil, err
	}
	if !follow {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	if bodyCap <= 0 {
		bodyCap = httpDefaultBodyCap
	}

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" && req.Header.Get("Content-Type") == "" {
		if strings.HasPrefix(strings.TrimSpace(body), "{") || strings.HasPrefix(strings.TrimSpace(body), "[") {
			req.Header.Set("Content-Type", "application/json")
		} else {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(bodyCap)+1))
	if readErr != nil {
		return nil, readErr
	}
	truncated := len(raw) > bodyCap
	if truncated {
		raw = raw[:bodyCap]
	}

	out := &httpResponse{
		ID: id, URL: rawURL, Method: method, Status: resp.StatusCode,
		Headers: resp.Header.Clone(), Body: raw, Elapsed: time.Since(start),
	}
	s.mu.Lock()
	if id == "" {
		id = fmt.Sprintf("r%d", len(s.last)+1)
		out.ID = id
	}
	s.last[id] = out
	if len(s.last) > httpCachedBodies {
		for k := range s.last {
			if k != id {
				delete(s.last, k)
				break
			}
		}
	}
	s.mu.Unlock()
	return out, nil
}

// renderHTTPResponse formats one response for the model: the useful fields, the
// cookie state (so the agent can see a session took hold) and a body snippet.
func renderHTTPResponse(r *httpResponse, bodyCap int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s → %d (%s)\n", r.Method, r.URL, r.Status, r.Elapsed.Round(time.Millisecond))
	if r.Headers != nil {
		// A curated list plus every X-* header: the fixed ones are the ones a probe
		// almost always needs, while X-* is where custom stacks leak version, cache
		// and debug state. Dumping all headers would bury both under CDN noise.
		keys := []string{"Content-Type", "Content-Length", "Location", "Server", "Set-Cookie", "WWW-Authenticate"}
		seen := map[string]bool{}
		for _, k := range keys {
			seen[strings.ToLower(k)] = true
		}
		var extra []string
		for k := range r.Headers {
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, "x-") && !seen[lk] {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		keys = append(keys, extra...)
		for _, k := range keys {
			if v := r.Headers.Get(k); v != "" {
				fmt.Fprintf(&b, "%s: %s\n", k, v)
			}
		}
	}
	fmt.Fprintf(&b, "response_id: %s | body_len: %d\n", r.ID, len(r.Body))
	if bodyCap <= 0 || bodyCap > len(r.Body) {
		bodyCap = len(r.Body)
	}
	if bodyCap > 0 {
		body := string(r.Body[:bodyCap])
		if len(r.Body) > bodyCap {
			body += "\n… (truncated)"
		}
		fmt.Fprintf(&b, "\n%s\n", body)
	}
	return b.String()
}

func (t *ToolSet) httpRequestTool() actool.CoreTool {
	reqSchema := map[string]any{
		"method":  str("HTTP method (default GET)"),
		"url":     str("Full URL including scheme"),
		"headers": obj(map[string]any{"Name": str("Header value")}),
		"body":    str("Request body (string); Content-Type is inferred when omitted"),
		"session": str("Session name to share cookies with (default: the task's main session)"),
	}
	schema := obj(reqSchema)
	schema["follow_redirects"] = boolp("Follow redirects (default true)")
	schema["timeout"] = intp("Timeout in seconds (default 30, max 300)")
	schema["body_cap"] = intp("Max body bytes returned (default 20000)")

	return writeTool("http_request",
		"Send one HTTP request from inside the platform and get a structured result: status, selected headers, body length, a body snippet and a response_id. "+
			"Use it instead of curl for anything you will read back, because it shares a cookie jar per session: log in once with session_login and every later request in the same session is already authenticated — that is what makes a login → IDOR → re-read chain work. "+
			"Egress goes through the same recording proxy as the shell, so the traffic is still captured and attributable. "+
			"Set follow_redirects=false when the Location header is the finding.",
		schema,
		func(ctx context.Context, args json.RawMessage) (actool.Result, error) {
			var in struct {
				Method  string            `json:"method"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
				Session string            `json:"session"`
				Follow  *bool             `json:"follow_redirects"`
				Timeout int               `json:"timeout"`
				BodyCap int               `json:"body_cap"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			follow := true
			if in.Follow != nil {
				follow = *in.Follow
			}
			resp, err := t.doHTTP(ctx, in.Session, "", in.Method, in.URL, in.Headers, in.Body, follow,
				time.Duration(in.Timeout)*time.Second, in.BodyCap)
			if err != nil {
				return actool.Errorf("request failed: " + err.Error()), nil
			}
			return actool.Text(renderHTTPResponse(resp, in.BodyCap)), nil
		})
}

func (t *ToolSet) sessionLoginTool() actool.CoreTool {
	return writeTool("session_login",
		"Log in once and keep the cookies in a named session, so later http_request calls in that session are already authenticated. "+
			"Accepts a form body or JSON depending on content_type. Report whether a cookie came back: a 200 without Set-Cookie usually means the credentials failed or the login is token-based in the body. "+
			"The password is used for this request only and is never echoed back.",
		obj(map[string]any{
			"session":      str("Session name to store the cookies in (default: the task's main session)"),
			"url":          str("Login endpoint URL"),
			"username":     str("Username or account"),
			"password":     str("Password"),
			"user_field":   str("Body field name for the account (default username)"),
			"pass_field":   str("Body field name for the password (default password)"),
			"extra_fields": obj(map[string]any{"Name": str("Value")}),
			"content_type": str("application/x-www-form-urlencoded (default) or application/json"),
			"method":       str("HTTP method (default POST)"),
		}),
		func(ctx context.Context, args json.RawMessage) (actool.Result, error) {
			var in struct {
				Session     string            `json:"session"`
				URL         string            `json:"url"`
				Username    string            `json:"username"`
				Password    string            `json:"password"`
				UserField   string            `json:"user_field"`
				PassField   string            `json:"pass_field"`
				ExtraFields map[string]string `json:"extra_fields"`
				ContentType string            `json:"content_type"`
				Method      string            `json:"method"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			if in.URL == "" {
				return actool.Errorf("url is required"), nil
			}
			if in.UserField == "" {
				in.UserField = "username"
			}
			if in.PassField == "" {
				in.PassField = "password"
			}
			if in.Method == "" {
				in.Method = http.MethodPost
			}
			fields := map[string]string{in.UserField: in.Username, in.PassField: in.Password}
			for k, v := range in.ExtraFields {
				fields[k] = v
			}
			headers := map[string]string{}
			var body string
			if strings.Contains(in.ContentType, "json") {
				raw, _ := json.Marshal(fields)
				body = string(raw)
				headers["Content-Type"] = "application/json"
			} else {
				form := url.Values{}
				for k, v := range fields {
					form.Set(k, v)
				}
				body = form.Encode()
				headers["Content-Type"] = "application/x-www-form-urlencoded"
			}

			resp, err := t.doHTTP(ctx, in.Session, "", in.Method, in.URL, headers, body, true, 0, httpDefaultBodyCap)
			if err != nil {
				return actool.Errorf("login failed: " + err.Error()), nil
			}
			s := t.httpSessionsFor(in.Session)
			u, _ := url.Parse(in.URL)
			cookies := s.jar.Cookies(u)
			var b strings.Builder
			fmt.Fprintf(&b, "login %s → %d\n", in.URL, resp.Status)
			if len(cookies) == 0 {
				b.WriteString("no cookie was stored: the login probably failed, or the session token comes back in the body — read the response and store it yourself.\n")
			} else {
				fmt.Fprintf(&b, "%d cookie(s) stored in session %q; later http_request calls with that session are authenticated:\n", len(cookies), in.Session)
				for _, c := range cookies {
					fmt.Fprintf(&b, "- %s (len %d)\n", c.Name, len(c.Value))
				}
				b.WriteString("(values are not echoed — use the session)")
			}
			if len(resp.Body) > 0 {
				snippet := string(resp.Body)
				if len(snippet) > 600 {
					snippet = snippet[:600] + "…"
				}
				fmt.Fprintf(&b, "\nbody snippet:\n%s\n", snippet)
			}
			return actool.Text(b.String()), nil
		})
}

// SetHTTPProxy wires the egress proxy used by the native HTTP tools, mirroring
// what the shell gets. A provider is used because the proxy can change per run.
func (t *ToolSet) SetHTTPProxy(provider func() string, caCertPath string) {
	t.httpProxy = provider
	t.httpCACert = caCertPath
}
