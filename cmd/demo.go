package main

import (
	gotls "crypto/tls"
	"fmt"
	"github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"httpmod"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func main() {
	httpmod.Apply()

	demoPlain()
	demoProxyied()

	httpmod.UTLSClientHelloID = tls.HelloFirefox_65

	demoProxyied()
}

func demoProxyied() {
	proxyUrl, err := url.Parse("http://107.161.50.58:7603/")
	if err != nil {
		panic(err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyUrl),
		},
	}

	req, err := http.NewRequest(http.MethodGet, "https://client.tlsfingerprint.io:8443/", nil)
	if err != nil {
		panic(err)
	}

	oh := new(httpmod.OrderedHeader)
	oh.Add("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	oh.Add("Accept-Encoding", "gzip, deflate")
	oh.Add("Accept-Language", "en-GB,en;q=0.5")
	oh.Add("Upgrade-Insecure-Requests", "1")
	oh.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/83.0.4103.116 Safari/537.36")
	oh.Apply(req.Header)

	r, err := httpClient.Do(req)
	if err != nil {
		panic(err)
	}

	rb, err := httputil.DumpResponse(r, true)
	if err != nil {
		panic(err)
	}

	fmt.Println(string(rb))
}

func demoPlain() {

	http2Client := http.Client{
		Transport: &http2.Transport{
			DialTLS: dialUTLS,
		},
	}
	req, err := http.NewRequest(http.MethodGet, "https://google.com/", nil)
	if err != nil {
		panic(err)
	}

	oh := new(httpmod.OrderedHeader)
	oh.Add("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	oh.Add("Accept-Encoding", "gzip, deflate")
	oh.Add("Accept-Language", "en-GB,en;q=0.5")
	oh.Add("Upgrade-Insecure-Requests", "1")
	oh.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/82.0.4183.121 Safari/537.36")
	oh.Apply(req.Header)

	r, err  := http2Client.Do(req)

	rb, err := httputil.DumpResponse(r, true)
	if err != nil {
		panic(err)
	}

	fmt.Println(string(rb))
}

func dialUTLS(network, addr string, cfg *gotls.Config) (net.Conn, error) {
	roll, err := tls.NewRoller()
	if err != nil {
		return nil, err
	}

	roll.HelloIDs = []tls.ClientHelloID{
		tls.HelloChrome_Auto,
		tls.HelloFirefox_Auto,
	}

	sni := strings.Split(addr, ":")[0]
	return roll.Dial(network, addr, sni)
}
