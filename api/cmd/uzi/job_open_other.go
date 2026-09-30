//go:build !unix

package main

// openNonblock is a no-op where O_NONBLOCK is not available.
const openNonblock = 0
