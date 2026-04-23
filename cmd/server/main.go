// Command server is the instant-lite-api entrypoint.
//
// It is intentionally thin: everything lives in internal/server so that
// future cmd/ binaries (e.g. migration tools, one-off jobs) can reuse
// the same building blocks without duplicating setup.
package main

import "instant.dev/lite/internal/server"

func main() {
	server.Run()
}
