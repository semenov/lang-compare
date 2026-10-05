package main

import (
	"fmt"
	"os"
)

var stopProf = func() {}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "Usage: lox [path]")
		os.Exit(64)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not open file \"%s\".\n", os.Args[1])
		os.Exit(74)
	}
	vm := newVM()
	code := vm.interpret(string(src))
	vm.out.Flush()
	stopProf()
	os.Exit(code)
}
