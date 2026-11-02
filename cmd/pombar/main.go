// Command pombar draws a shrinking pomodoro countdown bar on a screen edge.
//
// This is the M1 layer-shell spike: a fixed 60 s green bar on the bottom
// edge, redrawn every second.
package main

import (
	"flag"
	"fmt"
	"image/color"
	"os"
	"time"

	"github.com/clobrano/pomobar/internal/bar"
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

	const total = 60 * time.Second
	green := color.RGBA{0x4C, 0xAF, 0x50, 0xFF}

	b, err := bar.Open(bar.Options{Thickness: 4})
	if err != nil {
		fmt.Fprintln(os.Stderr, "pombar:", err)
		os.Exit(1)
	}
	defer b.Close()

	start := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		remaining := total - time.Since(start)
		if remaining <= 0 {
			return
		}
		if err := b.Draw(float64(remaining)/float64(total), green); err != nil {
			fmt.Fprintln(os.Stderr, "pombar:", err)
			return
		}
		select {
		case <-tick.C:
		case <-b.Done():
			fmt.Fprintln(os.Stderr, "pombar:", b.Err())
			return
		}
	}
}
