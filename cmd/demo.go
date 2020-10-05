package main

import (
	"httpmod"
	"net/http"
)

func main() {

	httpmod.Apply()

	req, err := http.NewRequest(http.MethodGet, "http://localhost:9090/banaan", nil)
	if err != nil {
		panic(err)
	}

	req.Header.Set("Custom-Header", "MyValue\nOtherKey: OtherValue")

	http.DefaultClient.Do(req)
}
