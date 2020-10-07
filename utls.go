package httpmod

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"

	utls "gitlab.com/yawning/utls.git"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

var httpRoundTripper = http.DefaultTransport.(*http.Transport)

// Copy the public fields (fields for which CanSet is true) from src to dst.
// src and dst must be pointers to the same type. We use this to make copies of
// httpRoundTripper. We cannot use struct assignment, because http.Transport
// contains private mutexes. The idea of using reflection to copy only the
// public fields comes from a post by Nick Craig-Wood:
// https://groups.google.com/d/msg/Golang-Nuts/SDiGYNVE8iY/89hRKTF4BAAJ
func copyPublicFields(dst, src interface{}) {
	if reflect.TypeOf(dst) != reflect.TypeOf(src) {
		panic("unequal types")
	}
	dstValue := reflect.ValueOf(dst).Elem()
	srcValue := reflect.ValueOf(src).Elem()
	for i := 0; i < dstValue.NumField(); i++ {
		if dstValue.Field(i).CanSet() {
			dstValue.Field(i).Set(srcValue.Field(i))
		}
	}
}

// Extract a host:port address from a URL, suitable for passing to net.Dial.
func addrForDial(url *url.URL) (string, error) {
	host := url.Hostname()
	// net/http would use golang.org/x/net/idna here, to convert a possible
	// internationalized domain name to ASCII.
	port := url.Port()
	if port == "" {
		// No port? Use the default for the scheme.
		switch url.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", fmt.Errorf("unsupported URL scheme %q", url.Scheme)
		}
	}
	return net.JoinHostPort(host, port), nil
}

// Analogous to tls.Dial. Connect to the given address and initiate a TLS
// handshake using the given ClientHelloID, returning the resulting connection.
func dialUTLS(network, addr string, cfg *utls.Config, clientHelloID *utls.ClientHelloID, forward proxy.Dialer) (*utls.UConn, error) {
	conn, err := forward.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(conn, cfg, *clientHelloID)
	if cfg == nil || cfg.ServerName == "" {
		serverName, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		uconn.SetSNI(serverName)
	}
	err = uconn.Handshake()
	if err != nil {
		return nil, err
	}
	return uconn, nil
}

// A http.RoundTripper that uses uTLS (with a specified Client Hello ID) to make
// TLS connections.
//
// Can only be reused among servers which negotiate the same ALPN.
type UTLSRoundTripper struct {
	sync.Mutex

	clientHelloID *utls.ClientHelloID
	config        *utls.Config
	proxyDialer   proxy.Dialer
	rt            http.RoundTripper

	// Transport for HTTP requests, which don't use uTLS.
	httpRT *http.Transport
}

func (rt *UTLSRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Scheme {
	case "http":
		// If http, we don't invoke uTLS; just pass it to an ordinary
		// http.Transport.
		return rt.httpRT.RoundTrip(req)
	case "https":
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q", req.URL.Scheme)
	}

	rt.Lock()
	defer rt.Unlock()

	if rt.rt == nil {
		// On the first call, make an http.Transport or http2.Transport
		// as appropriate.
		var err error
		rt.rt, err = makeRoundTripper(req.URL, rt.clientHelloID, rt.config, rt.proxyDialer)
		if err != nil {
			return nil, err
		}
	}
	// Forward the request to the internal http.Transport or http2.Transport.
	return rt.rt.RoundTrip(req)
}

func makeProxyDialer(proxyURL *url.URL, cfg *utls.Config, clientHelloID *utls.ClientHelloID) (proxy.Dialer, error) {
	var proxyDialer proxy.Dialer = proxy.Direct
	if proxyURL == nil {
		return proxyDialer, nil
	}

	proxyAddr, err := addrForDial(proxyURL)
	if err != nil {
		return nil, err
	}

	var auth *proxy.Auth
	if userpass := proxyURL.User; userpass != nil {
		auth = &proxy.Auth{
			User: userpass.Username(),
		}
		if password, ok := userpass.Password(); ok {
			auth.Password = password
		}
	}

	switch proxyURL.Scheme {
	case "socks5":
		proxyDialer, err = proxy.SOCKS5("tcp", proxyAddr, auth, proxyDialer)
	case "http":
		proxyDialer, err = ProxyHTTP("tcp", proxyAddr, auth, proxyDialer)
	case "https":
		// We use the same uTLS Config for TLS to the HTTPS proxy, as we
		// use for HTTPS connections through the tunnel. We make a clone
		// of the Config to avoid concurrent modification as the two
		// layers set the ServerName value.
		var cfgClone *utls.Config
		if cfg != nil {
			cfgClone = cfg.Clone()
		}
		proxyDialer, err = ProxyHTTPS("tcp", proxyAddr, auth, proxyDialer, cfgClone, clientHelloID)
	default:
		return nil, fmt.Errorf("cannot use proxy scheme %q with uTLS", proxyURL.Scheme)
	}

	return proxyDialer, err
}

func makeRoundTripper(url *url.URL, clientHelloID *utls.ClientHelloID, cfg *utls.Config, proxyDialer proxy.Dialer) (http.RoundTripper, error) {
	addr, err := addrForDial(url)
	if err != nil {
		return nil, err
	}

	// Connect to the given address, through a proxy if requested, and
	// initiate a TLS handshake using the given ClientHelloID. Return the
	// resulting connection.
	dial := func(network, addr string) (*utls.UConn, error) {
		return dialUTLS(network, addr, cfg, clientHelloID, proxyDialer)
	}

	bootstrapConn, err := dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	// Peek at what protocol we negotiated.
	protocol := bootstrapConn.ConnectionState().NegotiatedProtocol

	// Protects bootstrapConn.
	var lock sync.Mutex
	// This is the callback for future dials done by the internal
	// http.Transport or http2.Transport.
	dialTLS := func(network, addr string) (net.Conn, error) {
		lock.Lock()
		defer lock.Unlock()

		// On the first dial, reuse bootstrapConn.
		if bootstrapConn != nil {
			uconn := bootstrapConn
			bootstrapConn = nil
			return uconn, nil
		}

		// Later dials make a new connection.
		uconn, err := dial(network, addr)
		if err != nil {
			return nil, err
		}
		if uconn.ConnectionState().NegotiatedProtocol != protocol {
			return nil, fmt.Errorf("unexpected switch from ALPN %q to %q",
				protocol, uconn.ConnectionState().NegotiatedProtocol)
		}

		return uconn, nil
	}

	// Construct an http.Transport or http2.Transport depending on ALPN.
	switch protocol {
	case http2.NextProtoTLS:
		// Unfortunately http2.Transport does not expose the same
		// configuration options as http.Transport with regard to
		// timeouts, etc., so we are at the mercy of the defaults.
		// https://github.com/golang/go/issues/16581
		return &http2.Transport{
			DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
				// Ignore the *tls.Config parameter; use our
				// static cfg instead.
				return dialTLS(network, addr)
			},
		}, nil
	default:
		// With http.Transport, copy important default fields from
		// http.DefaultTransport, such as TLSHandshakeTimeout and
		// IdleConnTimeout.
		tr := &http.Transport{}
		copyPublicFields(tr, httpRoundTripper)
		tr.DialTLS = dialTLS
		return tr, nil
	}
}

// When you update this map, also update the man page in doc/meek-client.1.txt.
var clientHelloIDMap = map[string]*utls.ClientHelloID{
	// No HelloCustom: not useful for external configuration.
	// No HelloRandomized: doesn't negotiate consistent ALPN.
	"none":                  nil, // special case: disable uTLS
	"hellogolang":           nil, // special case: disable uTLS
	"hellorandomizedalpn":   &utls.HelloRandomizedALPN,
	"hellorandomizednoalpn": &utls.HelloRandomizedNoALPN,
	"hellofirefox_auto":     &utls.HelloFirefox_Auto,
	"hellofirefox_55":       &utls.HelloFirefox_55,
	"hellofirefox_56":       &utls.HelloFirefox_56,
	"hellofirefox_63":       &utls.HelloFirefox_63,
	"hellochrome_auto":      &utls.HelloChrome_Auto,
	"hellochrome_58":        &utls.HelloChrome_58,
	"hellochrome_62":        &utls.HelloChrome_62,
	"hellochrome_70":        &utls.HelloChrome_70,
	"helloios_auto":         &utls.HelloIOS_Auto,
	"helloios_11_1":         &utls.HelloIOS_11_1,
}

func NewUTLSRoundTripper(clientHelloID *utls.ClientHelloID, cfg *utls.Config, proxyURL *url.URL) (http.RoundTripper, error) {
	proxyDialer, err := makeProxyDialer(proxyURL, cfg, clientHelloID)
	if err != nil {
		return nil, err
	}

	// This special-case RoundTripper is used for HTTP requests, which don't
	// use uTLS but should use the specified proxy.
	httpRT := &http.Transport{}
	copyPublicFields(httpRT, httpRoundTripper)
	httpRT.Proxy = http.ProxyURL(proxyURL)

	return &UTLSRoundTripper{
		clientHelloID: clientHelloID,
		config:        cfg,
		proxyDialer:   proxyDialer,
		// rt will be set in the first call to RoundTrip.
		httpRT: httpRT,
	}, nil
}
