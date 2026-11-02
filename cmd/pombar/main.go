// Command pombar draws a shrinking pomodoro countdown bar on a screen edge.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("pombar", version)
		return
	}
	fmt.Fprintln(os.Stderr, "pombar: not implemented yet")
	os.Exit(1)
}
