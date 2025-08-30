package main

/*
#include <stdlib.h>

typedef struct {
    char* error;
} CallResult;
*/
import "C"
import "unsafe"

//export StartCall
func StartCall(cNumber *C.char) C.CallResult {
	number := C.GoString(cNumber)
	err := runCall(number)
	if err != nil {
		cstr := C.CString(err.Error())
		return C.CallResult{error: cstr}
	}
	return C.CallResult{error: nil}
}

//export FreeCString
func FreeCString(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

// NOTE: main() still exists in main.go for CLI usage; when building c-shared, it's ignored.
