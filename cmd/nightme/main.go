// Package main is the nightme daemon entrypoint.
package main

import "fmt"

// version is set at build time via -ldflags (see Makefile / scripts).
var version = "v0.1.0"

func main() {
	fmt.Printf("nightme %s - Local Bridge test mode\n", version)
}
