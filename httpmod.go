package httpmod

import (
	"bou.ke/monkey"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
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

//go:linkname customHeaderValidation vendor/golang.org/x/net/http/httpguts.ValidHeaderFieldValue
func customHeaderValidation(a string) bool

//go:linkname httpWriteSubset net/http.Header.writeSubset
func httpWriteSubset(h http.Header, w io.Writer, exclude map[string]bool, trace *httptrace.ClientTrace) error

func cursedWriteSubset(h http.Header, w io.Writer, exclude map[string]bool, trace *httptrace.ClientTrace) error {
	ws, ok := w.(io.StringWriter)
	if !ok {
		ws = stringWriter{w}
	}
	kvs, sorter := sortedKeyValues(h, exclude)
	var formattedVals []string
	for _, kv := range kvs {
		for _, v := range kv.values {
			v = textproto.TrimString(v)
			for _, s := range []string{kv.key, ": ", v, "\r\n"} {
				if _, err := ws.WriteString(s); err != nil {
					headerSorterPool.Put(sorter)
					return err
				}
			}
			if trace != nil && trace.WroteHeaderField != nil {
				formattedVals = append(formattedVals, v)
			}
		}
		if trace != nil && trace.WroteHeaderField != nil {
			trace.WroteHeaderField(kv.key, formattedVals)
			formattedVals = nil
		}
	}
	headerSorterPool.Put(sorter)
	return nil
}


var headerSorterPool = sync.Pool{
	New: func() interface{} { return new(headerSorter) },
}

// sortedKeyValues returns h's keys sorted in the returned kvs
// slice. The headerSorter used to sort is also returned, for possible
// return to headerSorterCache.
func sortedKeyValues(h http.Header, exclude map[string]bool) (kvs []keyValues, hs *headerSorter) {
	hs = headerSorterPool.Get().(*headerSorter)
	if cap(hs.kvs) < len(h) {
		hs.kvs = make([]keyValues, 0, len(h))
	}
	kvs = hs.kvs[:0]
	for k, vv := range h {
		if !exclude[k] {
			kvs = append(kvs, keyValues{k, vv})
		}
	}
	hs.kvs = kvs
	sort.Sort(hs)
	return kvs, hs
}

// A headerSorter implements sort.Interface by sorting a []keyValues
// by key. It's used as a pointer, so it can fit in a sort.Interface
// interface value without allocation.
type headerSorter struct {
	kvs []keyValues
}

func (s *headerSorter) Len() int           { return len(s.kvs) }
func (s *headerSorter) Swap(i, j int)      { s.kvs[i], s.kvs[j] = s.kvs[j], s.kvs[i] }
func (s *headerSorter) Less(i, j int) bool { return s.kvs[i].key < s.kvs[j].key }

type keyValues struct {
	key    string
	values []string
}

// stringWriter implements WriteString on a Writer.
type stringWriter struct {
	w io.Writer
}

func (w stringWriter) WriteString(s string) (n int, err error) {
	return w.w.Write([]byte(s))
}