// Package tour serves the grsh tutorial in a browser: the same eight
// chapters, the same verifiers, the same real shell session — with the
// terminal replaced by a page.
//
// It exists because the lesson engine was never really coupled to the
// REPL. The engine is a repl.Interceptor, four hooks around an input
// unit, and tutor.Driver calls those four hooks for a host that has no
// line editor. This package is that host: it turns an HTTP request into a
// line of input and a stream of output back.
//
//	browser                     tour.Server                tutor.Driver
//	───────────────────────     ──────────────────────     ─────────────────
//	POST /input {"line":…}  ──▶ visitor.submit       ──▶   Submit(line)
//	                                                        │ engine hooks
//	                                                        │ real Eval
//	  ◀── event: out  ────────  sink (SSE + replay)  ◀──    session stdout
//	  ◀── event: state ───────  View as JSON         ◀──    View()
//
// The page draws two surfaces from those two streams: a transcript that is
// the terminal, and a sidebar that is the lesson. The engine's step panel
// is routed away from the transcript (see tutor.DriverOptions.Panels)
// because the sidebar already says it — everything else the engine prints
// is a reply to something the student just did, and stays inline where it
// belongs.
//
// # This is a local tool
//
// The server runs commands, as the user, with the user's permissions. It
// binds to loopback and says no to anything else without an explicit
// flag, and that is not a security boundary — it is a reminder that there
// is no boundary to speak of. Do not put this on a network.
//
// Visitors are cheap but not free: each one holds a playground on disk and
// a live shell session, and they take turns to evaluate (see
// tutor's eval gate), so this comfortably serves a few tabs on one machine
// and nothing more ambitious than that.
package tour

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/grsh/internal/tutor"
	"github.com/rohanthewiz/rweb"
)

// Options configures a tour server.
type Options struct {
	// Addr is the listen address. Loopback unless AllowRemote is set.
	Addr string
	// AllowRemote permits a non-loopback bind. See the package comment for
	// why this is a flag and not a default.
	AllowRemote bool
	// Store persists progress across server restarts. Nil — the default —
	// keeps every visitor's place in memory only, which is deliberate: a
	// long-running server holding the shared ~/.grsh_tutor.db open would
	// silently disable resume for `grsh tutor`, and a browser tab keeps its
	// own session alive for as long as it is open anyway.
	Store tutor.Store
	// IdleTimeout reclaims a visitor whose browser has gone away: the
	// playground is removed and the session's jobs are dropped. Zero picks
	// a sensible default.
	IdleTimeout time.Duration
	Verbose     bool
	// Ready, when set, is closed as the server enters its listen loop.
	// A caller that binds to port 0 needs it: the port is not knowable
	// until the listener exists, and polling for it is a race dressed up
	// as a retry.
	Ready chan struct{}
}

const (
	defaultIdleTimeout = 30 * time.Minute
	// sidCookie names the visitor. A cookie rather than a URL parameter so
	// that a reload, a second window, and the SSE connection all land on
	// the same session without the page having to thread an id through
	// every request.
	sidCookie = "grsh_tour"
)

// Server is a running tour. Its zero value is not usable; call New.
type Server struct {
	opts Options
	web  *rweb.Server

	mu       sync.Mutex
	visitors map[string]*visitor
	closed   bool
}

// New builds the server and registers its routes. It does not listen.
func New(o Options) (*Server, error) {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:7654"
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = defaultIdleTimeout
	}
	if !o.AllowRemote {
		if err := requireLoopback(o.Addr); err != nil {
			return nil, err
		}
	}
	s := &Server{opts: o, visitors: map[string]*visitor{}}
	s.web = rweb.NewServer(rweb.ServerOptions{Address: o.Addr, Verbose: o.Verbose, ReadyChan: o.Ready})
	s.routes()
	return s, nil
}

// Addr is the address the server was configured to listen on. Once it is
// listening, Port is the one that was actually taken — they differ when
// the caller asked for port 0.
func (s *Server) Addr() string { return s.opts.Addr }

// Port is the listening port, valid only after Ready has been closed.
func (s *Server) Port() string { return s.web.GetListenPort() }

// Run listens and serves until the process ends or the listener fails.
func (s *Server) Run() error {
	go s.reap()
	return s.web.Run()
}

// Close tears down every visitor: playgrounds removed, sessions dropped.
// A tour's whole state is disposable by design, so this is all that
// shutdown means.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	vs := make([]*visitor, 0, len(s.visitors))
	for _, v := range s.visitors {
		vs = append(vs, v)
	}
	clear(s.visitors)
	s.mu.Unlock()
	for _, v := range vs {
		v.close()
	}
}

// routes wires the surface. It is deliberately small: two streams (input
// down, output up) plus the handful of reads the page needs to draw.
func (s *Server) routes() {
	s.web.Get("/", s.page)
	s.web.Get("/app.css", asset("app.css", "text/css; charset=utf-8"))
	s.web.Get("/app.js", asset("app.js", "text/javascript; charset=utf-8"))

	s.web.Get("/events", s.events)
	s.web.Get("/state", s.state)
	s.web.Get("/classify", s.classify)
	s.web.Post("/input", s.input)
	s.web.Post("/interrupt", s.interrupt)
	s.web.Post("/reset", s.reset)
}

// page serves the shell of the application and, on a first visit, mints
// the session. Building the visitor here rather than lazily on /events
// means the playground exists — and any failure to create it is reported
// as an HTTP error — before the browser starts streaming.
func (s *Server) page(ctx rweb.Context) error {
	if _, err := s.visitorFor(ctx, true); err != nil {
		return ctx.SetStatus(500).WriteString("could not start a tour: " + err.Error())
	}
	return ctx.WriteHTMLBytes(indexHTML)
}

// events is the output stream: a replay of everything the visitor has
// missed, then the live feed.
//
// Attaching closes any stream this visitor already had, which is what
// makes a reload work — the browser's old EventSource is gone, and its
// server side has to be told, since an SSE connection is only detectably
// dead once something tries to write to it.
func (s *Server) events(ctx rweb.Context) error {
	v, err := s.visitorFor(ctx, false)
	if err != nil {
		return ctx.SetStatus(404).WriteString("no session — reload the page")
	}
	// SetSSE writes the whole header set the stream needs. It notably does
	// not write a Content-Encoding, which is the correct behaviour and worth
	// knowing: rweb <= v0.1.26 sent `Content-Encoding: text/plain` — a media
	// type where a content CODING belongs — and browsers and curl drop a body
	// whose coding they cannot decode, so headers arrived and events never
	// did. We carried an `identity` override here until rweb v0.1.28 removed
	// the header upstream. TestTourSendsADecodableStream still guards it,
	// since a dependency could reintroduce it and no Go client would notice.
	return ctx.SetSSE(v.sink.attach(), "out")
}

// state is the sidebar's initial draw, and its recovery path if the page
// ever gets ahead of the stream.
func (s *Server) state(ctx rweb.Context) error {
	v, err := s.visitorFor(ctx, false)
	if err != nil {
		return ctx.SetStatus(404).WriteJSON(map[string]string{"error": "no session"})
	}
	return ctx.WriteJSON(v.view())
}

// input takes one physical line. It returns the resulting View directly
// rather than leaving the page to wait for the broadcast: the reply is the
// acknowledgement that the line was consumed, and a page that re-enabled
// its input on an event instead would have nothing to do if the event were
// dropped.
func (s *Server) input(ctx rweb.Context) error {
	v, err := s.visitorFor(ctx, false)
	if err != nil {
		return ctx.SetStatus(404).WriteJSON(map[string]string{"error": "no session"})
	}
	var body struct {
		Line string `json:"line"`
	}
	if err := json.Unmarshal(ctx.Request().Body(), &body); err != nil {
		return ctx.SetStatus(400).WriteJSON(map[string]string{"error": "bad request body"})
	}
	// A submitted line may contain newlines if the page ever pastes a
	// block; feeding them one at a time is what the driver expects, and it
	// keeps a paste indistinguishable from typing.
	view := v.submit(strings.Split(body.Line, "\n"))
	v.sink.state(view)
	return ctx.WriteJSON(view)
}

// interrupt is the stop button. It does not take the visitor's lock — the
// one moment it is worth anything is while a command is running and that
// lock is held.
func (s *Server) interrupt(ctx rweb.Context) error {
	v, err := s.visitorFor(ctx, false)
	if err != nil {
		return ctx.SetStatus(404).WriteJSON(map[string]string{"error": "no session"})
	}
	return ctx.WriteJSON(map[string]bool{"signalled": v.d.Interrupt()})
}

// reset throws the session away and starts the curriculum over with a
// clean playground — the browser's version of quitting and relaunching.
func (s *Server) reset(ctx rweb.Context) error {
	v, err := s.visitorFor(ctx, false)
	if err != nil {
		return ctx.SetStatus(404).WriteJSON(map[string]string{"error": "no session"})
	}
	if err := v.restart(s.opts.Store); err != nil {
		return ctx.SetStatus(500).WriteJSON(map[string]string{"error": err.Error()})
	}
	view := v.view()
	v.sink.state(view)
	return ctx.WriteJSON(view)
}

// classify answers the live classifier lane as the student types. It is
// called per keystroke (debounced), so it does nothing but read.
func (s *Server) classify(ctx rweb.Context) error {
	v, err := s.visitorFor(ctx, false)
	if err != nil {
		return ctx.SetStatus(404).WriteJSON(map[string]string{"verdict": ""})
	}
	return ctx.WriteJSON(map[string]string{"verdict": v.classify(ctx.Request().QueryParam("src"))})
}

// visitorFor resolves the session cookie, optionally minting a new
// session. Requests other than the page never mint one: an /input from a
// stale tab should be told to reload, not silently handed a fresh
// curriculum from chapter one.
func (s *Server) visitorFor(ctx rweb.Context, create bool) (*visitor, error) {
	id, _ := ctx.GetCookie(sidCookie)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("server is shutting down")
	}
	if v, ok := s.visitors[id]; ok {
		v.touch()
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()
	if !create {
		return nil, fmt.Errorf("no session")
	}

	// Built outside the lock: opening a chapter writes a playground to
	// disk, and holding the map's lock across that would serialize every
	// first visit behind it.
	v, err := newVisitor(s.opts.Store)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		v.close()
		return nil, fmt.Errorf("server is shutting down")
	}
	s.visitors[v.id] = v
	s.mu.Unlock()
	if err := ctx.SetCookie(sidCookie, v.id); err != nil {
		return nil, err
	}
	return v, nil
}

// reap removes visitors whose browsers have gone. Without it a tab closed
// mid-chapter would leave its playground on disk and its background jobs
// running until the process ended.
func (s *Server) reap() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for range tick.C {
		cutoff := time.Now().Add(-s.opts.IdleTimeout)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		var dead []*visitor
		for id, v := range s.visitors {
			if v.lastSeen().Before(cutoff) {
				dead = append(dead, v)
				delete(s.visitors, id)
			}
		}
		s.mu.Unlock()
		for _, v := range dead {
			v.close()
		}
	}
}

// requireLoopback is the guard behind AllowRemote. It resolves the host
// rather than string-matching "localhost", because the interesting mistake
// is `--addr :7654`, which is every interface and looks like nothing at
// all.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("%q listens on every interface; the tour runs shell commands as you — "+
			"use 127.0.0.1 or pass --allow-remote if you really mean it", addr)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("%q is not a loopback address; the tour runs shell commands as you — "+
				"pass --allow-remote if you really mean it", addr)
		}
	}
	return nil
}

// asset serves one embedded file. The page's CSS and JS are separate
// requests rather than inlined so a browser reload during development
// picks them up the way any other static file would.
func asset(name, contentType string) rweb.Handler {
	return func(ctx rweb.Context) error {
		body, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return ctx.SetStatus(404).WriteString("not found")
		}
		ctx.Response().SetHeader("Content-Type", contentType)
		return ctx.Bytes(body)
	}
}

// jsonLine encodes v as a single line of JSON.
//
// SSE frames data line by line, and rweb writes exactly one `data:` line
// per event — so a payload containing a raw newline would break the frame
// and everything after it. JSON escaping is what makes the transcript's
// newlines safe to send, which is why even a bare chunk of text travels as
// an object.
func jsonLine(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"encode failed"}`
	}
	return string(b)
}

var _ io.Writer = (*sink)(nil)
