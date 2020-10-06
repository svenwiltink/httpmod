package httpmod

import (
	"bufio"
	gotls "crypto/tls"
	"errors"
	tls "github.com/refraction-networking/utls"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
	_ "unsafe"
)

var UTLSClientHelloID = tls.HelloChrome_83

//go:linkname stdlibAddTls net/http.(*persistConn).addTLS
func stdlibAddTls(pconn *persistConn, name string, trace *httptrace.ClientTrace) error

func patchedAddTls(pconn *persistConn, name string, trace *httptrace.ClientTrace) error {
	plainConn := pconn.conn

	tlsConn := tls.UClient(plainConn, &tls.Config{ServerName: name}, UTLSClientHelloID)

	errc := make(chan error, 2)
	var timer *time.Timer // for canceling TLS handshake
	if d := pconn.t.TLSHandshakeTimeout; d != 0 {
		timer = time.AfterFunc(d, func() {
			errc <- errors.New("tls handshake timeout")
		})
	}
	go func() {
		err := tlsConn.Handshake()
		if timer != nil {
			timer.Stop()
		}
		errc <- err
	}()
	if err := <-errc; err != nil {
		plainConn.Close()
		return err
	}

	cs := tlsConn.ConnectionState()

	// copy utls to tls struct. Can't be converted directly because of an unexported field
	gcs := gotls.ConnectionState{
		Version:                     cs.Version,
		HandshakeComplete:           cs.HandshakeComplete,
		DidResume:                   cs.DidResume,
		CipherSuite:                 cs.CipherSuite,
		NegotiatedProtocol:          cs.NegotiatedProtocol,
		NegotiatedProtocolIsMutual:  cs.NegotiatedProtocolIsMutual,
		ServerName:                  cs.ServerName,
		PeerCertificates:            cs.PeerCertificates,
		VerifiedChains:              cs.VerifiedChains,
		SignedCertificateTimestamps: cs.SignedCertificateTimestamps,
		OCSPResponse:                cs.OCSPResponse,
		TLSUnique:                   cs.TLSUnique,
	}

	pconn.tlsState = &gcs
	pconn.conn = tlsConn
	return nil
}

// Everything below is yoinked from net/http to make patching possible

// connectMethodKey is the map key version of connectMethod, with a
// stringified proxy URL (or the empty string) instead of a pointer to
// a URL.
type connectMethodKey struct {
	proxy, scheme, addr string
	onlyH1              bool
}

// responseAndError is how the goroutine reading from an HTTP/1 server
// communicates with the goroutine doing the RoundTrip.
type responseAndError struct {
	res *http.Response // else use this response (see res method)
	err error
}

type requestAndChan struct {
	req *http.Request
	ch  chan responseAndError // unbuffered; always send in select on callerGone

	// whether the Transport (as opposed to the user client code)
	// added the Accept-Encoding gzip header. If the Transport
	// set it, only then do we transparently decode the gzip.
	addedGzip bool

	// Optional blocking chan for Expect: 100-continue (for send).
	// If the request has an "Expect: 100-continue" header and
	// the server responds 100 Continue, readLoop send a value
	// to writeLoop via this chan.
	continueCh chan<- struct{}

	callerGone <-chan struct{} // closed when roundTrip caller has returned
}

// A writeRequest is sent by the readLoop's goroutine to the
// writeLoop's goroutine to write a request while the read loop
// concurrently waits on both the write response and the server's
// reply.
type writeRequest struct {
	req *transportRequest
	ch  chan<- error

	// Optional blocking chan for Expect: 100-continue (for receive).
	// If not nil, writeLoop blocks sending request body until
	// it receives from this chan.
	continueCh <-chan struct{}
}

// transportRequest is a wrapper around a *Request that adds
// optional extra headers to write and stores any error to return
// from roundTrip.
type transportRequest struct {
	*http.Request                        // original request, not to be mutated
	extra         http.Header            // extra headers to write, or nil
	trace         *httptrace.ClientTrace // optional

	mu  sync.Mutex // guards err
	err error      // first setError value for mapRoundTripError to consider
}

// persistConn wraps a connection, usually a persistent one
// (but may be used for non-keep-alive requests as well)
type persistConn struct {
	// alt optionally specifies the TLS NextProto RoundTripper.
	// This is used for HTTP/2 today and future protocols later.
	// If it's non-nil, the rest of the fields are unused.
	alt http.RoundTripper

	t         *http.Transport
	cacheKey  connectMethodKey
	conn      net.Conn
	tlsState  *gotls.ConnectionState
	br        *bufio.Reader       // from conn
	bw        *bufio.Writer       // to conn
	nwrite    int64               // bytes written
	reqch     chan requestAndChan // written by roundTrip; read by readLoop
	writech   chan writeRequest   // written by roundTrip; read by writeLoop
	closech   chan struct{}       // closed when conn closed
	isProxy   bool
	sawEOF    bool  // whether we've seen EOF from conn; owned by readLoop
	readLimit int64 // bytes allowed to be read; owned by readLoop
	// writeErrCh passes the request write error (usually nil)
	// from the writeLoop goroutine to the readLoop which passes
	// it off to the res.Body reader, which then uses it to decide
	// whether or not a connection can be reused. Issue 7569.
	writeErrCh chan error

	writeLoopDone chan struct{} // closed when write loop ends

	// Both guarded by Transport.idleMu:
	idleAt    time.Time   // time it last become idle
	idleTimer *time.Timer // holding an AfterFunc to close it

	mu                   sync.Mutex // guards following fields
	numExpectedResponses int
	closed               error // set non-nil when conn is closed, before closech is closed
	canceledErr          error // set non-nil if conn is canceled
	broken               bool  // an error has happened on this connection; marked broken so it's not reused.
	reused               bool  // whether conn has had successful request/response and is being reused.
	// mutateHeaderFunc is an optional func to modify extra
	// headers on each outbound request before it's written. (the
	// original Request given to RoundTrip is not modified)
	mutateHeaderFunc func(header http.Header)
}
