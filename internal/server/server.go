// Package server exposes node power control over HTTP.
//
// Two interfaces are served:
//
//   - A small REST API, wire-compatible with the original node-power Python
//     service: POST /nodes/{name}/power {"action":"on|off|cycle"}.
//   - POST /rpc, the JSON contract of bmclib's "rpc" provider, which lets
//     Tinkerbell's Rufio treat a PoE port as a BMC.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/conallob/unifi-power-controller/internal/power"
)

// Power is the behaviour the server needs from the PoE controller.
type Power interface {
	On(ctx context.Context, node string) error
	Off(ctx context.Context, node string) error
	Cycle(ctx context.Context, node string) error
	State(ctx context.Context, node string) (power.State, error)
}

// Options configures a Server.
type Options struct {
	Power  Power
	Logger *slog.Logger

	// HMAC secrets for POST /rpc. If both are empty, /rpc is unauthenticated.
	SHA256Secrets []string
	SHA512Secrets []string
	SignedHeaders []string

	// Metrics, if set, is served at GET /metrics.
	Metrics http.Handler
}

// Server is an http.Handler.
type Server struct {
	opts Options
	log  *slog.Logger
	mux  *http.ServeMux
}

const maxBody = 1 << 16

// New builds the handler.
func New(o Options) *Server {
	s := &Server{opts: o, log: o.Logger, mux: http.NewServeMux()}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.mux.HandleFunc("GET /nodes/{name}/power", s.getPower)
	s.mux.HandleFunc("POST /nodes/{name}/power", s.postPower)
	s.mux.HandleFunc("POST /rpc", s.rpc)
	if o.Metrics != nil {
		s.mux.Handle("GET /metrics", o.Metrics)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// --- REST -------------------------------------------------------------

func (s *Server) postPower(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("name")
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	var err error
	switch body.Action {
	case "on":
		err = s.opts.Power.On(r.Context(), node)
	case "off":
		err = s.opts.Power.Off(r.Context(), node)
	case "cycle":
		err = s.opts.Power.Cycle(r.Context(), node)
	default:
		http.Error(w, "action must be on, off, or cycle", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.log.Error("power action failed", "node", node, "action", body.Action, "err", err)
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	s.log.Info("power action", "node", node, "action", body.Action)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getPower(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("name")
	st, err := s.opts.Power.State(r.Context(), node)
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(st)})
}

func statusFor(err error) int {
	if errors.Is(err, power.ErrUnknownNode) {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

// --- bmclib rpc provider ---------------------------------------------

type rpcRequest struct {
	ID     int64           `json:"id"`
	Host   string          `json:"host"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     int64     `json:"id"`
	Host   string    `json:"host"`
	Result any       `json:"result,omitempty"`
	Error  *rpcError `json:"error,omitempty"`
}

// JSON-RPC style error codes returned in the response body.
const (
	codeInvalid     = 400
	codeNotFound    = 404
	codeUnsupported = 501
	codeBackend     = 502
)

func (s *Server) rpc(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !s.verify(r, body) {
		s.log.Warn("rpc: bad or missing signature", "remote", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	resp := rpcResponse{ID: req.ID, Host: req.Host}
	result, rerr := s.dispatch(r.Context(), req)
	if rerr != nil {
		resp.Error = rerr
		s.log.Error("rpc failed", "host", req.Host, "method", req.Method, "code", rerr.Code, "msg", rerr.Message)
	} else {
		resp.Result = result
		s.log.Info("rpc", "host", req.Host, "method", req.Method, "params", string(req.Params))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "ping":
		return "pong", nil

	case "getPowerState":
		st, err := s.opts.Power.State(ctx, req.Host)
		if err != nil {
			return nil, toRPCError(err)
		}
		return string(st), nil

	case "setPowerState":
		var p struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.State == "" {
			return nil, &rpcError{codeInvalid, "params.state is required"}
		}
		var err error
		switch strings.ToLower(p.State) {
		case "on":
			err = s.opts.Power.On(ctx, req.Host)
		case "off", "hardoff", "hard-off":
			err = s.opts.Power.Off(ctx, req.Host)
		case "cycle", "reset":
			// PoE has no soft reset; a hard cycle is the closest equivalent.
			err = s.opts.Power.Cycle(ctx, req.Host)
		default:
			// "soft" needs an OS or BMC; a PoE port can only cut power.
			return nil, &rpcError{codeUnsupported, "unsupported power state: " + p.State}
		}
		if err != nil {
			return nil, toRPCError(err)
		}
		return "ok", nil

	case "setBootDevice":
		// There is no BMC to program a one-time boot device. Nodes are
		// expected to network-boot by default (Tinkerbell then decides what to
		// serve), so acknowledge rather than fail the whole Rufio Job.
		return "ok", nil

	default:
		return nil, &rpcError{codeUnsupported, "unsupported method: " + req.Method}
	}
}

func toRPCError(err error) *rpcError {
	if errors.Is(err, power.ErrUnknownNode) {
		return &rpcError{codeNotFound, err.Error()}
	}
	return &rpcError{codeBackend, err.Error()}
}

// verify checks the bmclib HMAC signature headers. The signed payload is the
// request body followed by the values of any configured SignedHeaders.
func (s *Server) verify(r *http.Request, body []byte) bool {
	if len(s.opts.SHA256Secrets) == 0 && len(s.opts.SHA512Secrets) == 0 {
		return true
	}
	payload := append([]byte(nil), body...)
	for _, h := range s.opts.SignedHeaders {
		payload = append(payload, strings.Join(r.Header.Values(h), "")...)
	}
	return matches(r.Header.Get("X-BMCLIB-Signature-256"), payload, s.opts.SHA256Secrets, sha256.New) ||
		matches(r.Header.Get("X-BMCLIB-Signature-512"), payload, s.opts.SHA512Secrets, sha512.New)
}

// matches reports whether any comma-separated signature in header equals the
// HMAC of payload under any secret. A leading "sha256=" style prefix is
// accepted; Rufio adds it by default.
func matches(header string, payload []byte, secrets []string, newHash func() hash.Hash) bool {
	if header == "" || len(secrets) == 0 {
		return false
	}
	var got [][]byte
	for _, sig := range strings.Split(header, ",") {
		sig = strings.TrimSpace(sig)
		if i := strings.IndexByte(sig, '='); i >= 0 {
			sig = sig[i+1:]
		}
		if b, err := hex.DecodeString(sig); err == nil {
			got = append(got, b)
		}
	}
	ok := false
	for _, secret := range secrets {
		m := hmac.New(newHash, []byte(secret))
		m.Write(payload)
		want := m.Sum(nil)
		for _, g := range got {
			if hmac.Equal(g, want) { // constant time; keep scanning all
				ok = true
			}
		}
	}
	return ok
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	// bmclib rejects responses without a Content-Length (chunked), so set it.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(code)
	_, _ = w.Write(b)
}
