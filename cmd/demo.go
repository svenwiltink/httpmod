package main

import (
	"fmt"
	tls "gitlab.com/yawning/utls.git"
	"httpmod"
	"net/http"
	"net/http/httputil"
	"net/url"
)

func main() {
	httpmod.Apply()

	demoPlain("https://postman-echo.com/get")
	demoPlain("http://postman-echo.com/get")
	demoProxyied("https://www.genx.co.nz/")
	demoProxyied("https://httpbin.org/anything")
	demoProxyied("https://ja3er.com/json")
	demoProxyied("https://client.tlsfingerprint.io:8443/")
}

func demoProxyied(target string) {
	proxyUrl, err := url.Parse("http://107.161.50.58:7603/")
	if err != nil {
		panic(err)
	}

	tripper, err := httpmod.NewUTLSRoundTripper(&tls.HelloFirefox_Auto, nil, proxyUrl)
	httpClient := &http.Client{
		Transport: tripper,
	}

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		panic(err)
	}

	oh := make(httpmod.OrderedHeader)
	oh.Add("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	oh.Add("Accept-Encoding", "gzip, deflate")
	oh.Add("Accept-Language", "en-GB,en;q=0.5")
	oh.Add("Upgrade-Insecure-Requests", "1")
	oh.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/83.0.4103.116 Safari/537.36")

	req.Header = http.Header(oh)


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

func demoPlain(target string) {

	tripper, err := httpmod.NewUTLSRoundTripper(&tls.HelloFirefox_Auto, nil, nil)
	httpClient := &http.Client{
		Transport: tripper,
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		panic(err)
	}

	oh := make(httpmod.OrderedHeader)
	oh.Add("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	oh.Add("Accept-Encoding", "gzip, deflate")
	oh.Add("Accept-Language", "en-GB,en;q=0.5")
	oh.Add("SUP", "HEY")
	oh.Add("Upgrade-Insecure-Requests", "1")
	oh.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/82.0.4183.121 Safari/537.36")

	req.Header = http.Header(oh)

	r, err  := httpClient.Do(req)
	if err != nil {
		panic(err)
	}

	rb, err := httputil.DumpResponse(r, true)
	if err != nil {
		panic(err)
	}

	fmt.Println(string(rb))
}
