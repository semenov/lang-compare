package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "index" {
		if err := runIndex(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, "index:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "serve" {
		if err := runServe(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 4 && os.Args[1] == "bench" {
		prof := ""
		if len(os.Args) > 4 {
			prof = os.Args[4]
		}
		if err := runBench(os.Args[2], os.Args[3], 3, prof); err != nil {
			fmt.Fprintln(os.Stderr, "bench:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintln(os.Stderr, "usage: search index <corpus.jsonl> <index-dir> | search serve <index-dir>")
	os.Exit(2)
}
