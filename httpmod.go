package httpmod

import (
	"bou.ke/monkey"
	"net/http"
	"reflect"
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

	guard = monkey.Patch(stdlibHeaderWriteSubset, patchedHeaderWriteSubset)
	patchGuards = append(patchGuards, guard)

	guard = monkey.Patch(stdlibAddTls, patchedAddTls)
	patchGuards = append(patchGuards, guard)
}

func Remove() {
	for _, guard := range patchGuards {
		guard.Unpatch()
	}
}
