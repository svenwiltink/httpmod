package httpmod

import (
	"bufio"
	"crypto/tls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"io"
	"net"
	"sync"
	"time"
	_ "unsafe"
)

var (
	TransportDefaultConnFlow uint32 = 1 << 30

	// appears to be the initial windows size in wireshark
	TransportDefaultStreamFlow uint32 = 4 << 20

	InitialWindowSize uint32 = 65535

	InitialHeaderTableSize uint32 = 4096

	MaxHeaderListSize uint32 = 10 << 20

	SettingEnablePush uint32 = 0

	clientPreface = []byte(http2.ClientPreface)
)

//go:linkname stdlibNewClientConn golang.org/x/net/http2.(*Transport).newClientConn
func stdlibNewClientConn(t *http2.Transport, c net.Conn, singleUse bool) (*ClientConn, error)

func patchedNewClientConn(t *http2.Transport, c net.Conn, singleUse bool) (*ClientConn, error) {
	cc := &ClientConn{
		t:                     t,
		tconn:                 c,
		readerDone:            make(chan struct{}),
		nextStreamID:          1,
		maxFrameSize:          16 << 10,           // spec default
		initialWindowSize:     65535,              // spec default
		maxConcurrentStreams:  1000,               // "infinite", per spec. 1000 seems good enough.
		peerMaxHeaderListSize: 0xffffffffffffffff, // "infinite", per spec. Use 2^64-1 instead.
		streams:               make(map[uint32]*clientStream),
		singleUse:             singleUse,
		wantSettingsAck:       true,
		pings:                 make(map[[8]byte]chan struct{}),
	}
	if d := stdLibIdleConnTimeout(t); d != 0 {
		cc.idleTimeout = d
		cc.idleTimer = time.AfterFunc(d, onIdleTimeout)
	}
	if http2.VerboseLogs {
		vlogf(t, "http2: Transport creating client conn %p to %v", cc, c.RemoteAddr())
	}

	cc.cond = sync.NewCond(&cc.mu)

	flowAdd(&cc.flow, int32(InitialWindowSize))


	cc.bw = bufio.NewWriter(stickyErrWriter{c, &cc.werr})
	cc.br = bufio.NewReader(c)
	cc.fr = http2.NewFramer(cc.bw, cc.br)
	cc.fr.ReadMetaHeaders = hpack.NewDecoder(InitialHeaderTableSize, nil)
	cc.fr.MaxHeaderListSize = MaxHeaderListSize

	cc.henc = hpack.NewEncoder(&cc.hbuf)

	if t.AllowHTTP {
		cc.nextStreamID = 3
	}

	if cs, ok := c.(connectionStater); ok {
		state := cs.ConnectionState()
		cc.tlsState = &state
	}

	initialSettings := []http2.Setting{
		{ID: http2.SettingEnablePush, Val: SettingEnablePush},
		{ID: http2.SettingInitialWindowSize, Val: TransportDefaultStreamFlow},
	}

	initialSettings = append(initialSettings, http2.Setting{ID: http2.SettingMaxHeaderListSize, Val: MaxHeaderListSize})

	cc.bw.Write(clientPreface)
	cc.fr.WriteSettings(initialSettings...)
	cc.fr.WriteWindowUpdate(0, TransportDefaultConnFlow)

	flowAdd(&cc.inflow, int32(TransportDefaultConnFlow+InitialWindowSize))
	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}

	go readLoop(cc)
	return cc, nil
}

//go:linkname stdLibIdleConnTimeout golang.org/x/net/http2.(*Transport).idleConnTimeout
func stdLibIdleConnTimeout(t *http2.Transport) time.Duration

//go:linkname maxHeaderListSize golang.org/x/net/http2.(*Transport).maxHeaderListSize
func maxHeaderListSize(t *http2.Transport) uint32

//go:linkname vlogf golang.org/x/net/http2.(*Transport).vlogf
func vlogf(t *http2.Transport, format string, args ...interface{})

//go:linkname readLoop golang.org/x/net/http2.(*ClientConn).readLoop
func readLoop(cc *ClientConn)

//go:linkname onIdleTimeout golang.org/x/net/http2.(*ClientConn).onIdleTimeout
func onIdleTimeout()

//go:linkname flowAdd golang.org/x/net/http2.(*flow).add
func flowAdd(f *flow, n int32) bool

type connectionStater interface {
	ConnectionState() tls.ConnectionState
}

type stickyErrWriter struct {
	w   io.Writer
	err *error
}

func (sew stickyErrWriter) Write(p []byte) (n int, err error) {
	if *sew.err != nil {
		return 0, *sew.err
	}
	n, err = sew.w.Write(p)
	*sew.err = err
	return
}