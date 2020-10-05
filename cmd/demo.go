package main

import (
	"fmt"
	"httpmod"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func main() {
	httpmod.Apply()

	req, err := http.NewRequest(http.MethodPost, "https://postman-echo.com/post", strings.NewReader(url.Values{"HEY": []string{"YOU","SUBSCRIBE"}}.Encode()))
	if err != nil {
		panic(err)
	}

	oh := new(httpmod.OrderedHeader)
	oh.Add("WOW", "so cool!")
	oh.Add("Absolutely", "Amazing!")
	oh.Add("User-Agent", "Bananentaart")
	oh.Apply(req.Header)

	r, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}

	rb, err := httputil.DumpResponse(r, true)
	if err != nil {
		panic(err)
	}

	fmt.Println(string(rb))
}
