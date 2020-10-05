package httpmod

import (
	"bou.ke/monkey"
	"io"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"sort"
	"sync"
	_ "unsafe"
)

var patchGuards []*monkey.PatchGuard

func Apply() {

	// disable header validation
	guard := monkey.Patch(customHeaderValidation, func(s string) bool {
		return true
	})
	patchGuards = append(patchGuards, guard)

	req := http.Request{}

	// allow any value to be set
	guard = monkey.PatchInstanceMethod(reflect.TypeOf(req.Header), "Set", func(h http.Header, k, v string) {
		h[k] = []string{v}
	})
	patchGuards = append(patchGuards, guard)

	guard = monkey.Patch(httpWriteSubset, cursedWriteSubset)
	patchGuards = append(patchGuards, guard)
}

func Remove() {
	for _, guard := range patchGuards {
		guard.Unpatch()
	}
}

type OrderedHeader []string

func (oh *OrderedHeader) Add(key, value string) {
	*oh = append(*oh, key + ": " + value)
}

func (oh *OrderedHeader) Apply(header http.Header) {
	// set to empty string to stdlib doesn't output one
	header["User-Agent"] = []string{}
	header["Custom-Headers"] = *oh
}

//go:linkname customHeaderValidation vendor/golang.org/x/net/http/httpguts.ValidHeaderFieldValue
func customHeaderValidation(a string) bool

//go:linkname httpWriteSubset net/http.Header.writeSubset
func httpWriteSubset(h http.Header, w io.Writer, exclude map[string]bool, trace *httptrace.ClientTrace) error

func cursedWriteSubset(h http.Header, w io.Writer, exclude map[string]bool, trace *httptrace.ClientTrace) error {
	ws, ok := w.(io.StringWriter)
	if !ok {
		ws = stringWriter{w}
	}

	customHeader, exists := h["Custom-Headers"]
	if !exists {
		return nil
	}

	for _, v := range customHeader {
		for _, s := range []string{v, "\r\n"} {
			if _, err := ws.WriteString(s); err != nil {
				return err
			}
		}
	}
	return nil
}

// stringWriter implements WriteString on a Writer.
type stringWriter struct {
	w io.Writer
}

func (w stringWriter) WriteString(s string) (n int, err error) {
	return w.w.Write([]byte(s))
}