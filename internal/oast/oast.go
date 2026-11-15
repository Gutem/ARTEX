// Package oast provides an out-of-band callback channel: the only way to decide
// whether a vulnerability that produces no echo actually fired.
//
// Blind SSRF, blind command injection, blind XXE and JNDI-style lookups never
// answer in the response. The single usable criterion is "did the target come
// back to an address we control". Without such a channel an agent can only guess,
// which is why report_finding forbids registering a finding it did not trigger.
//
// Two transports, independently enabled:
//   - HTTP: a listener where every token gets a unique URL; a hit is recorded on
//     any request to that URL.
//   - DNS: an authoritative responder (miekg/dns) that records every query and
//     answers A records. DNS often leaks even when egress to HTTP ports is blocked.
//
// Public reachability is the operator's problem: PublicBase must be the address
// the TARGET can reach, not the listen address. The listen address is what the
// process binds; the two differ behind NAT, a proxy or a tunnel.
package oast

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Options configures the listeners. An empty address disables that transport.
type Options struct {
	HTTPAddr string // e.g. 0.0.0.0:8888; empty disables HTTP
	DNSAddr  string // e.g. 0.0.0.0:5353; empty disables DNS
	AnswerIP string // A record answered for DNS queries (default 127.0.0.1)

	// PublicBase is the base address reachable FROM THE TARGET, e.g.
	// https://oast.example.com or http://203.0.113.10:8888. When empty, token
	// URLs fall back to the listen address, which a remote target usually cannot
	// reach — fine for local tests only.
	PublicBase string

	MaxHits int           // hits kept per token (default 200)
	TTL     time.Duration // token lifetime (default 24h)
}

// Token is the handle for one out-of-band probe.
type Token struct {
	ID        string    `json:"id"`
	HTTPURL   string    `json:"http_url,omitempty"`
	DNSHost   string    `json:"dns_host,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Hit is one recorded callback.
type Hit struct {
	TokenID string    `json:"token_id"`
	Proto   string    `json:"proto"`  // http|dns
	Source  string    `json:"source"` // remote address
	Detail  string    `json:"detail"` // request line or queried name + type
	At      time.Time `json:"at"`
}

type tokenState struct {
	tok     Token
	hits    []Hit
	expires time.Time
}

// Server is the out-of-band channel. Safe for concurrent use.
type Server struct {
	opts Options

	mu     sync.RWMutex
	tokens map[string]*tokenState

	httpSrv  *http.Server
	httpAddr string
	dnsSrv   *dns.Server
	dnsTCP   *dns.Server
	dnsAddr  string
	closed   bool
}

// New builds a stopped server. Call Start to bind the listeners.
func New(opts Options) *Server {
	if opts.MaxHits <= 0 {
		opts.MaxHits = 200
	}
	if opts.TTL <= 0 {
		opts.TTL = 24 * time.Hour
	}
	if strings.TrimSpace(opts.AnswerIP) == "" {
		opts.AnswerIP = "127.0.0.1"
	}
	return &Server{opts: opts, tokens: map[string]*tokenState{}}
}

// Start binds the enabled listeners. HTTPAddr and DNSAddr report the addresses
// actually bound, including a random port when the option asked for :0.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("oast: server already closed")
	}
	if s.opts.HTTPAddr != "" {
		ln, err := net.Listen("tcp", s.opts.HTTPAddr)
		if err != nil {
			return fmt.Errorf("oast: http listen: %w", err)
		}
		s.httpAddr = ln.Addr().String()
		mux := http.NewServeMux()
		mux.HandleFunc("/", s.handleHTTP)
		s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = s.httpSrv.Serve(ln) }()
	}
	if s.opts.DNSAddr != "" {
		pc, err := net.ListenPacket("udp", s.opts.DNSAddr)
		if err != nil {
			return fmt.Errorf("oast: dns listen: %w", err)
		}
		s.dnsAddr = pc.LocalAddr().String()
		h := dns.HandlerFunc(s.handleDNS)
		s.dnsSrv = &dns.Server{PacketConn: pc, Handler: h}
		go func() { _ = s.dnsSrv.ActivateAndServe() }()
		if tcp, terr := net.Listen("tcp", s.dnsAddr); terr == nil {
			// TCP is required when a response exceeds 512 bytes or the client
			// retries over TCP; without it some resolvers silently give up.
			s.dnsTCP = &dns.Server{Listener: tcp, Handler: h}
			go func() { _ = s.dnsTCP.ActivateAndServe() }()
		}
	}
	return nil
}

// HTTPAddr returns the bound HTTP address ("" when disabled).
func (s *Server) HTTPAddr() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.httpAddr }

// DNSAddr returns the bound DNS address ("" when disabled).
func (s *Server) DNSAddr() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.dnsAddr }

// NewToken mints a token plus the URL and hostname to inject into the payload.
func (s *Server) NewToken() Token {
	id := randHex(8)
	t := Token{ID: id, CreatedAt: time.Now()}

	base := strings.TrimRight(strings.TrimSpace(s.opts.PublicBase), "/")
	if base != "" {
		t.HTTPURL = base + "/" + id
		if u, err := url.Parse(base); err == nil && u.Host != "" {
			t.DNSHost = fmt.Sprintf("%s.%s", id, u.Hostname())
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if t.HTTPURL == "" {
		t.HTTPURL = s.httpURLFor(id)
	}
	if s.dnsAddr != "" && t.DNSHost == "" {
		t.DNSHost = s.dnsHostFor(id)
	}
	if t.HTTPURL == "" && t.DNSHost == "" {
		return Token{}
	}
	s.tokens[id] = &tokenState{tok: t, expires: time.Now().Add(s.opts.TTL)}
	return t
}

// httpURLFor builds a token URL from the listen address, replacing a wildcard
// bind with loopback so a local test still works.
func (s *Server) httpURLFor(id string) string {
	if s.httpAddr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(s.httpAddr)
	if err != nil {
		return fmt.Sprintf("http://%s/%s", s.httpAddr, id)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "80" {
		return fmt.Sprintf("http://%s/%s", host, id)
	}
	return fmt.Sprintf("http://%s:%s/%s", host, port, id)
}

// dnsHostFor builds "<token>.<host>[:port]" from the DNS listen address.
func (s *Server) dnsHostFor(id string) string {
	host, port, err := net.SplitHostPort(s.dnsAddr)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	name := fmt.Sprintf("%s.%s", id, host)
	if port != "" && port != "53" {
		name = fmt.Sprintf("%s:%s", name, port)
	}
	return name
}

// Hits returns a copy of the callbacks recorded for one token.
func (s *Server) Hits(id string) []Hit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.tokens[id]
	if st == nil {
		return nil
	}
	out := make([]Hit, len(st.hits))
	copy(out, st.hits)
	return out
}

// AllHits returns every recorded callback, token order unspecified.
func (s *Server) AllHits() []Hit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Hit
	for _, st := range s.tokens {
		out = append(out, st.hits...)
	}
	return out
}

// Close stops the listeners.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	hs, ds, dt := s.httpSrv, s.dnsSrv, s.dnsTCP
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if hs != nil {
		_ = hs.Shutdown(ctx)
	}
	if ds != nil {
		_ = ds.Shutdown()
	}
	if dt != nil {
		_ = dt.Shutdown()
	}
	return nil
}

// record stores one hit, dropping expired tokens and capping per-token history so
// a target that loops cannot grow memory without bound.
func (s *Server) record(id string, h Hit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.tokens[id]
	if st == nil || time.Now().After(st.expires) {
		return
	}
	if len(st.hits) >= s.opts.MaxHits {
		return
	}
	st.hits = append(st.hits, h)
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.SplitN(strings.Trim(r.URL.Path, "/"), "/", 2)[0]
	s.record(id, Hit{TokenID: id, Proto: "http", Source: clientIP(r), Detail: r.Method + " " + r.URL.String(), At: time.Now()})
	// A harmless 200 keeps the target's flow from aborting on an error, so the
	// exploitation path we are testing stays intact.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleDNS(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)
	if len(req.Question) > 0 {
		q := req.Question[0]
		id := strings.SplitN(strings.ToLower(q.Name), ".", 2)[0]
		s.record(id, Hit{TokenID: id, Proto: "dns", Source: w.RemoteAddr().String(), Detail: q.Name + " " + dns.TypeToString[q.Qtype], At: time.Now()})
		if q.Qtype == dns.TypeA {
			if rr, err := dns.NewRR(fmt.Sprintf("%s 60 IN A %s", q.Name, s.opts.AnswerIP)); err == nil {
				m.Answer = append(m.Answer, rr)
			}
		}
	}
	_ = w.WriteMsg(m)
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:n*2]
	}
	return hex.EncodeToString(b)
}
