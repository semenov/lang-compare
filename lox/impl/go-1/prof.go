//go:build prof

package main

import (
	"os"
	"runtime/pprof"
)

func init() {
	if p := os.Getenv("LOXPROF"); p != "" {
		f, _ := os.Create(p)
		pprof.StartCPUProfile(f)
		stopProf = func() { pprof.StopCPUProfile(); f.Close() }
	}
}
