// Package exit implements the DO-side HTTP handler. Apps Script POSTs
// AES-encrypted frame batches here; we decrypt, demux by session_id, dial real
// upstream targets on SYN, pump bytes between net.Conn and session, and
// long-poll the response so downstream bytes get delivered with low latency.
package exit

import (
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/kianmhz/relay-tunnel/internal/frame"
	"github.com/kianmhz/relay-tunnel/internal/session"
)

const (
	// LongPollWindow is how long the handler holds open a request waiting for
	// downstream bytes. Must be well under Apps Script's UrlFetchApp timeout
	// (~60s default) and the script execution limit (6m).
	LongPollWindow = 25 * time.Second

	// MaxFramePayload caps the bytes per downstream frame (matches carrier).
	MaxFramePayload = 32 * 1024

	// upstreamReadBuf is the chunk size for reading from real net.Conn before
	// pushing to session.EnqueueTx (which then chunks into frames).
	upstreamReadBuf = 32 * 1024
)

// Config is the DO server's configuration.
type Config struct {
	ListenAddr string // "0.0.0.0:8443"
	AESKeyHex  string // 64-char hex
}

// Server holds the per-process session state.
type Server struct {
	cfg  Config
	aead *frame.Crypto

	mu       sync.Mutex
	sessions map[[frame.SessionIDLen]byte]*session.Session

	activity chan struct{} // buffered len 1; coalesces "session has new tx" signals
}

// New constructs an exit Server.
func New(cfg Config) (*Server, error) {
	aead, err := frame.NewCryptoFromHexKey(cfg.AESKeyHex)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:      cfg,
		aead:     aead,
		sessions: make(map[[frame.SessionIDLen]byte]*session.Session),
		activity: make(chan struct{}, 1),
	}, nil
}

// ListenAndServe blocks. It binds an HTTP listener on cfg.ListenAddr with one
// route, POST /tunnel, that handles batched encrypted frames.
func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel", s.handleTunnel)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	httpSrv := &http.Server{
		Addr:        s.cfg.ListenAddr,
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout intentionally generous — long-poll responses can take
		// up to LongPollWindow to start writing.
		WriteTimeout: LongPollWindow + 10*time.Second,
	}
	log.Printf("[exit] listening on %s", s.cfg.ListenAddr)
	return httpSrv.ListenAndServe()
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[exit] read body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	rxFrames, err := frame.DecodeBatch(s.aead, body)
	if err != nil {
		log.Printf("[exit] decode batch: %v", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	for _, f := range rxFrames {
		s.routeIncoming(f)
	}

	// Long-poll: wait for any downstream bytes, up to LongPollWindow.
	deadline := time.Now().Add(LongPollWindow)
	for {
		txFrames := s.drainAll()
		if len(txFrames) > 0 {
			respBody, err := frame.EncodeBatch(s.aead, txFrames)
			if err != nil {
				log.Printf("[exit] encode response: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(respBody)
			s.gcDoneSessions()
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Empty response (still a valid base64-encoded zero-frame batch).
			respBody, _ := frame.EncodeBatch(s.aead, nil)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(respBody)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.activity:
			// loop and drain
		case <-time.After(remaining):
			// loop one more time, then exit on next iteration
		}
	}
}

// routeIncoming routes one incoming frame to its session, creating the session
// (and dialing upstream) if this is a SYN.
func (s *Server) routeIncoming(f *frame.Frame) {
	s.mu.Lock()
	sess, exists := s.sessions[f.SessionID]
	s.mu.Unlock()

	if !exists {
		if !f.HasFlag(frame.FlagSYN) {
			log.Printf("[exit] frame for unknown session (no SYN), dropping")
			return
		}
		var err error
		sess, err = s.openSession(f.SessionID, f.Target)
		if err != nil {
			log.Printf("[exit] dial %s: %v", f.Target, err)
			return
		}
	}
	sess.ProcessRx(f)
}

// openSession dials the upstream target, creates a Session for the given ID,
// registers it, and spawns the bidirectional pump goroutines.
func (s *Server) openSession(id [frame.SessionIDLen]byte, target string) (*session.Session, error) {
	upstream, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		return nil, err
	}
	sess := session.New(id, target, false)
	sess.OnTx = s.kick

	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	log.Printf("[exit] new session %x -> %s", id[:4], target)

	// Upstream → session.EnqueueTx (downstream direction).
	go func() {
		defer upstream.Close()
		buf := make([]byte, upstreamReadBuf)
		for {
			n, err := upstream.Read(buf)
			if n > 0 {
				sess.EnqueueTx(buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("[exit] upstream read %x: %v", id[:4], err)
				}
				sess.RequestClose()
				return
			}
		}
	}()

	// session.RxChan → upstream.Write (upstream direction).
	go func() {
		for data := range sess.RxChan {
			if _, err := upstream.Write(data); err != nil {
				log.Printf("[exit] upstream write %x: %v", id[:4], err)
				_ = upstream.Close()
				return
			}
		}
		_ = upstream.Close()
	}()

	return sess, nil
}

func (s *Server) drainAll() []*frame.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*frame.Frame
	for _, sess := range s.sessions {
		out = append(out, sess.DrainTx(MaxFramePayload)...)
	}
	return out
}

func (s *Server) gcDoneSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.IsDone() {
			delete(s.sessions, id)
		}
	}
}

func (s *Server) kick() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}
