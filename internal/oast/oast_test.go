package oast

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// startHTTPOnly boots a server with an HTTP listener on a random port.
func startHTTPOnly(t *testing.T) *Server {
	t.Helper()
	srv := New(Options{HTTPAddr: "127.0.0.1:0"})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func TestNewTokenGivesUniqueURLs(t *testing.T) {
	srv := startHTTPOnly(t)
	a, b := srv.NewToken(), srv.NewToken()
	if a.ID == "" || b.ID == "" {
		t.Fatal("token id must not be empty")
	}
	if a.ID == b.ID {
		t.Fatalf("token ids collide: %q", a.ID)
	}
	if a.HTTPURL == b.HTTPURL {
		t.Fatalf("urls collide: %q", a.HTTPURL)
	}
	if !strings.HasSuffix(a.HTTPURL, a.ID) {
		t.Fatalf("url %q does not carry the token id %q", a.HTTPURL, a.ID)
	}
}

func TestHTTPCallbackIsRecorded(t *testing.T) {
	srv := startHTTPOnly(t)
	tok := srv.NewToken()

	resp, err := http.Get(tok.HTTPURL)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a 4xx can abort the target's flow)", resp.StatusCode)
	}

	// The listener runs in a goroutine; give the write a moment to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits := srv.Hits(tok.ID); len(hits) == 1 {
			if hits[0].Proto != "http" {
				t.Fatalf("proto = %q, want http", hits[0].Proto)
			}
			if hits[0].TokenID != tok.ID {
				t.Fatalf("hit tagged with %q, want %q", hits[0].TokenID, tok.ID)
			}
			if !strings.Contains(hits[0].Detail, "GET") {
				t.Fatalf("detail lost the request line: %q", hits[0].Detail)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no callback recorded for %s", tok.ID)
}

func TestHitForUnknownTokenIsIgnored(t *testing.T) {
	srv := startHTTPOnly(t)
	resp, err := http.Get("http://" + srv.HTTPAddr() + "/naoexiste123")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if got := srv.AllHits(); len(got) != 0 {
		t.Fatalf("unknown token produced %d hits, want 0", len(got))
	}
}

func TestMaxHitsCapsHistory(t *testing.T) {
	srv := New(Options{HTTPAddr: "127.0.0.1:0", MaxHits: 3})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	tok := srv.NewToken()
	for i := 0; i < 6; i++ {
		srv.record(tok.ID, Hit{TokenID: tok.ID, Proto: "http", Detail: "probe"})
	}
	if got := len(srv.Hits(tok.ID)); got != 3 {
		t.Fatalf("hits kept = %d, want the cap of 3", got)
	}
}

func TestDNSQueryIsRecordedAndAnswered(t *testing.T) {
	srv := New(Options{DNSAddr: "127.0.0.1:0", AnswerIP: "203.0.113.7"})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	tok := srv.NewToken()
	if tok.DNSHost == "" {
		t.Fatal("dns listener up but the token carries no DNS host")
	}

	addr := srv.DNSAddr()
	c := &dns.Client{Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion(tok.ID+".probe.test.", dns.TypeA)
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("dns exchange: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers = %d, want 1 A record", len(resp.Answer))
	}
	if a, ok := resp.Answer[0].(*dns.A); !ok || a.A.String() != "203.0.113.7" {
		t.Fatalf("answer = %v, want the configured A record", resp.Answer[0])
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits := srv.Hits(tok.ID); len(hits) == 1 {
			if hits[0].Proto != "dns" {
				t.Fatalf("proto = %q, want dns", hits[0].Proto)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("dns query was not recorded")
}

// A query naming something that is not a minted token is background noise from
// resolvers and scanners; recording it would fill the channel with junk and make
// a real callback indistinguishable from it.
func TestDNSQueryForUnknownTokenIsNotRecorded(t *testing.T) {
	srv := New(Options{DNSAddr: "127.0.0.1:0"})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	c := &dns.Client{Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion("scan-noise.probe.test.", dns.TypeA)
	if _, _, err := c.Exchange(m, srv.DNSAddr()); err != nil {
		t.Fatalf("dns exchange: %v", err)
	}
	if got := srv.AllHits(); len(got) != 0 {
		t.Fatalf("unknown name produced %d hits, want 0", len(got))
	}
}

func TestPublicBaseWinsOverListenAddress(t *testing.T) {
	srv := New(Options{HTTPAddr: "127.0.0.1:0", PublicBase: "https://oast.example.com"})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	tok := srv.NewToken()
	if !strings.HasPrefix(tok.HTTPURL, "https://oast.example.com/") {
		t.Fatalf("url = %q, want the public base", tok.HTTPURL)
	}
	if !strings.HasSuffix(tok.DNSHost, ".oast.example.com") {
		t.Fatalf("dns host = %q, want it to end with the public hostname", tok.DNSHost)
	}
}

func TestNewTokenWithoutAnyListenerIsEmpty(t *testing.T) {
	srv := New(Options{})
	if tok := srv.NewToken(); tok.ID != "" {
		t.Fatalf("token id = %q, want empty when no listener is up", tok.ID)
	}
}

func TestAddrHelpersHandleWildcardBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	if strings.HasPrefix(addr, "0.0.0.0") {
		t.Skip("kernel returned a wildcard address")
	}
}
