package httpmod

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "unsafe"
)

type OrderedHeader http.Header

func (oh OrderedHeader) Add(key, value string) {
	oh["Custom-Header-Order"] = append(oh["Custom-Header-Order"], key)
	oh[key] = append(oh[key], value)
}

//go:linkname customHeaderValidation vendor/golang.org/x/net/http/httpguts.ValidHeaderFieldValue
func customHeaderValidation(a string) bool

//go:linkname stdlibEncodeHeaders golang.org/x/net/http2.(*ClientConn).encodeHeaders
func stdlibEncodeHeaders(cc *ClientConn, req *http.Request, addGzipHeader bool, trailers string, contentLength int64) ([]byte, error)

// mostly YOINKED from http2. Only changing the header order
func patchedEncodeHeaders(cc *ClientConn, req *http.Request, addGzipHeader bool, trailers string, contentLength int64) ([]byte, error) {
	cc.hbuf.Reset()

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	host, err := httpguts.PunycodeHostPort(host)
	if err != nil {
		return nil, err
	}

	var path string
	if req.Method != "CONNECT" {
		path = req.URL.RequestURI()
		if !validPseudoPath(path) {
			orig := path
			path = strings.TrimPrefix(path, req.URL.Scheme+"://"+host)
			if !validPseudoPath(path) {
				if req.URL.Opaque != "" {
					return nil, fmt.Errorf("invalid request :path %q from URL.Opaque = %q", orig, req.URL.Opaque)
				} else {
					return nil, fmt.Errorf("invalid request :path %q", orig)
				}
			}
		}
	}

	// Check for any invalid headers and return an error before we
	// potentially pollute our hpack state. (We want to be able to
	// continue to reuse the hpack encoder for future requests)
	for k, vv := range req.Header {
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, fmt.Errorf("invalid HTTP header name %q", k)
		}
		for _, v := range vv {
			if !httpguts.ValidHeaderFieldValue(v) {
				return nil, fmt.Errorf("invalid HTTP header value %q for header %q", v, k)
			}
		}
	}

	enumerateHeaders := func(f func(name, value string)) {
		// 8.1.2.3 Request Pseudo-Header Fields
		// The :path pseudo-header field includes the path and query parts of the
		// target URI (the path-absolute production and optionally a '?' character
		// followed by the query production (see Sections 3.3 and 3.4 of
		// [RFC3986]).
		f(":authority", host)
		m := req.Method
		if m == "" {
			m = http.MethodGet
		}
		f(":method", m)
		if req.Method != "CONNECT" {
			f(":path", path)
			f(":scheme", req.URL.Scheme)
		}
		if trailers != "" {
			f("trailer", trailers)
		}

		var didUA bool
		for k, vv := range req.Header {
			if strings.EqualFold(k, "host") || strings.EqualFold(k, "content-length") {
				// Host is :authority, already sent.
				// Content-Length is automatic, set below.
				continue
			} else if strings.EqualFold(k, "connection") || strings.EqualFold(k, "proxy-connection") ||
				strings.EqualFold(k, "transfer-encoding") || strings.EqualFold(k, "upgrade") ||
				strings.EqualFold(k, "keep-alive") {
				// Per 8.1.2.2 Connection-Specific Header
				// Fields, don't send connection-specific
				// fields. We have already checked if any
				// are error-worthy so just ignore the rest.
				continue
			} else if strings.EqualFold(k, "user-agent") {
				// Match Go's http1 behavior: at most one
				// User-Agent. If set to nil or empty string,
				// then omit it. Otherwise if not mentioned,
				// include the default (below).
				didUA = true
				if len(vv) < 1 {
					continue
				}
				vv = vv[:1]
				if vv[0] == "" {
					continue
				}
			} else if strings.EqualFold(k, "cookie") {
				// Per 8.1.2.5 To allow for better compression efficiency, the
				// Cookie header field MAY be split into separate header fields,
				// each with one or more cookie-pairs.
				for _, v := range vv {
					for {
						p := strings.IndexByte(v, ';')
						if p < 0 {
							break
						}
						f("cookie", v[:p])
						p++
						// strip space after semicolon if any.
						for p+1 <= len(v) && v[p] == ' ' {
							p++
						}
						v = v[p:]
					}
					if len(v) > 0 {
						f("cookie", v)
					}
				}
				continue
			}

			for _, v := range vv {
				f(k, v)
			}
		}
		if shouldSendReqContentLength(req.Method, contentLength) {
			f("content-length", strconv.FormatInt(contentLength, 10))
		}
		if addGzipHeader {
			f("accept-encoding", "gzip")
		}
		if !didUA {
			f("user-agent", "Go-Client")
		}
	}

	// Do a first pass over the headers counting bytes to ensure
	// we don't exceed cc.peerMaxHeaderListSize. This is done as a
	// separate pass before encoding the headers to prevent
	// modifying the hpack state.
	hlSize := uint64(0)
	enumerateHeaders(func(name, value string) {
		if name == "Custom-Header-Order" {
			return
		}

		hf := hpack.HeaderField{Name: name, Value: value}
		hlSize += uint64(hf.Size())
	})

	if hlSize > cc.peerMaxHeaderListSize {
		return nil, errors.New("max header list size exceeded")
	}

	headersToSend := make(map[string][]string)

	// enumerate the headers to send the http2 headers
	enumerateHeaders(func(name, value string) {
		// always send "http2" headers first
		if name[0] == ':' {
			lowKey := strings.ToLower(name)
			writeHeader(cc, lowKey, value)
			return
		}

		// don't want to send this one
		if name == "Custom-Header-Order" {
			return
		}
		headersToSend[name] = append(headersToSend[name], value)
	})

	// don't change anything if custom headers haven't been used
	headerOrder, ok := req.Header["Custom-Header-Order"]
	if !ok {
		enumerateHeaders(func(name, value string) {
			lowKey := strings.ToLower(name)
			writeHeader(cc, lowKey, value)
		})
	} else {
		for _, name := range headerOrder {
			for _, value := range headersToSend[name] {
				lowKey := strings.ToLower(name)
				writeHeader(cc, lowKey, value)
			}
		}
	}

	return cc.hbuf.Bytes(), nil
}
//go:linkname writeHeader golang.org/x/net/http2.(*ClientConn).writeHeader
func writeHeader(cc *ClientConn, name, value string)

//go:linkname stdlibHeaderWriteSubset net/http.Header.writeSubset
func stdlibHeaderWriteSubset(h http.Header, w io.Writer, exclude map[string]bool, trace *httptrace.ClientTrace) error

func patchedHeaderWriteSubset(h http.Header, w io.Writer, exclude map[string]bool, trace *httptrace.ClientTrace) error {
	ws, ok := w.(io.StringWriter)
	if !ok {
		ws = stringWriter{w}
	}

	customOrder, exists := h["Custom-Header-Order"]
	if !exists {
		return nil
	}

	for _, key := range customOrder {
		for _, value := range h[key] {
			for _, s := range []string{key, ": ", value,  "\r\n"} {
				if _, err := ws.WriteString(s); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// anything below this point was yoined from http2


// ClientConn is the state of a single HTTP/2 client connection to an
// HTTP/2 server.
type ClientConn struct {
	t         *http2.Transport
	tconn     net.Conn             // usually *tls.Conn, except specialized impls
	tlsState  *tls.ConnectionState // nil only for specialized impls
	reused    uint32               // whether conn is being reused; atomic
	singleUse bool                 // whether being used for a single http.Request

	// readLoop goroutine fields:
	readerDone chan struct{} // closed on error
	readerErr  error         // set before readerDone is closed

	idleTimeout time.Duration // or 0 for never
	idleTimer   *time.Timer

	mu              sync.Mutex // guards following
	cond            *sync.Cond // hold mu; broadcast on flow/closed changes
	flow            flow       // our conn-level flow control quota (cs.flow is per stream)
	inflow          flow       // peer's conn-level flow control
	closing         bool
	closed          bool
	wantSettingsAck bool                     // we sent a SETTINGS frame and haven't heard back
	goAway          *http2.GoAwayFrame             // if non-nil, the GoAwayFrame we received
	goAwayDebug     string                   // goAway frame's debug data, retained as a string
	streams         map[uint32]*clientStream // client-initiated
	nextStreamID    uint32
	pendingRequests int                       // requests blocked and waiting to be sent because len(streams) == maxConcurrentStreams
	pings           map[[8]byte]chan struct{} // in flight ping data to notification channel
	bw              *bufio.Writer
	br              *bufio.Reader
	fr              *http2.Framer
	lastActive      time.Time
	lastIdle        time.Time // time last idle
	// Settings from peer: (also guarded by mu)
	maxFrameSize          uint32
	maxConcurrentStreams  uint32
	peerMaxHeaderListSize uint64
	initialWindowSize     uint32

	hbuf    bytes.Buffer // HPACK encoder writes into this
	henc    *hpack.Encoder
	freeBuf [][]byte

	wmu  sync.Mutex // held while writing; acquire AFTER mu if holding both
	werr error      // first write error that has occurred
}


// clientStream is the state for a single HTTP/2 stream. One of these
// is created for each Transport.RoundTrip call.
type clientStream struct {
	cc            *ClientConn
	req           *http.Request
	trace         *httptrace.ClientTrace // or nil
	ID            uint32
	resc          chan resAndError
	bufPipe       pipe // buffered pipe with the flow-controlled response payload
	startedWrite  bool // started request body write; guarded by cc.mu
	requestedGzip bool
	on100         func() // optional code to run if get a 100 continue response

	flow        flow  // guarded by cc.mu
	inflow      flow  // guarded by cc.mu
	bytesRemain int64 // -1 means unknown; owned by transportResponseBody.Read
	readErr     error // sticky read error; owned by transportResponseBody.Read
	stopReqBody error // if non-nil, stop writing req body; guarded by cc.mu
	didReset    bool  // whether we sent a RST_STREAM to the server; guarded by cc.mu

	peerReset chan struct{} // closed on peer reset
	resetErr  error         // populated before peerReset is closed

	done chan struct{} // closed when stream remove from cc.streams map; close calls guarded by cc.mu

	// owned by clientConnReadLoop:
	firstByte    bool  // got the first response byte
	pastHeaders  bool  // got first MetaHeadersFrame (actual headers)
	pastTrailers bool  // got optional second MetaHeadersFrame (trailers)
	num1xx       uint8 // number of 1xx responses seen

	trailer    http.Header  // accumulated trailers
	resTrailer *http.Header // client's Response.Trailer
}

// flow is the flow control window's size.
type flow struct {
	_ incomparable

	// n is the number of DATA bytes we're allowed to send.
	// A flow is kept both on a conn and a per-stream.
	n int32

	// conn points to the shared connection-level flow that is
	// shared by all streams on that conn. It is nil for the flow
	// that's on the conn directly.
	conn *flow
}

// incomparable is a zero-width, non-comparable type. Adding it to a struct
// makes that struct also non-comparable, and generally doesn't add
// any size (as long as it's first).
type incomparable [0]func()

// pipe is a goroutine-safe io.Reader/io.Writer pair. It's like
// io.Pipe except there are no PipeReader/PipeWriter halves, and the
// underlying buffer is an interface. (io.Pipe is always unbuffered)
type pipe struct {
	mu       sync.Mutex
	c        sync.Cond     // c.L lazily initialized to &p.mu
	b        pipeBuffer    // nil when done reading
	unread   int           // bytes unread when done
	err      error         // read error once empty. non-nil means closed.
	breakErr error         // immediate read error (caller doesn't see rest of b)
	donec    chan struct{} // closed on error
	readFn   func()        // optional code to run in Read before error
}

type pipeBuffer interface {
	Len() int
	io.Writer
	io.Reader
}

type resAndError struct {
	_   incomparable
	res *http.Response
	err error
}

// shouldSendReqContentLength reports whether the http2.Transport should send
// a "content-length" request header. This logic is basically a copy of the net/http
// transferWriter.shouldSendContentLength.
// The contentLength is the corrected contentLength (so 0 means actually 0, not unknown).
// -1 means unknown.
func shouldSendReqContentLength(method string, contentLength int64) bool {
	if contentLength > 0 {
		return true
	}
	if contentLength < 0 {
		return false
	}
	// For zero bodies, whether we send a content-length depends on the method.
	// It also kinda doesn't matter for http2 either way, with END_STREAM.
	switch method {
	case "POST", "PUT", "PATCH":
		return true
	default:
		return false
	}
}

func validPseudoPath(v string) bool {
	return (len(v) > 0 && v[0] == '/') || v == "*"
}

// stringWriter implements WriteString on a Writer.
type stringWriter struct {
	w io.Writer
}

func (w stringWriter) WriteString(s string) (n int, err error) {
	return w.w.Write([]byte(s))
}