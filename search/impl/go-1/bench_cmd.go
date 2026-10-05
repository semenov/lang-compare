package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime/pprof"
	"sort"
	"time"
)

// runBench is a development aid: times every query in a file in-process.
func runBench(dir, qfile string, reps int, prof string) error {
	ix, err := openIndex(dir)
	if err != nil {
		return err
	}
	f, err := os.Open(qfile)
	if err != nil {
		return err
	}
	var qs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		qs = append(qs, sc.Text())
	}
	if prof != "" {
		pf, _ := os.Create(prof)
		pprof.StartCPUProfile(pf)
		defer pprof.StopCPUProfile()
	}
	type r struct {
		d time.Duration
		q string
	}
	var rs []r
	var sum time.Duration
	for _, q := range qs {
		t0 := time.Now()
		for i := 0; i < reps; i++ {
			ix.search(q, 10)
		}
		d := time.Since(t0) / time.Duration(reps)
		sum += d
		rs = append(rs, r{d, q})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].d > rs[j].d })
	for _, x := range rs[:min(12, len(rs))] {
		fmt.Printf("%8.2fms %q\n", float64(x.d.Microseconds())/1000, x.q)
	}
	fmt.Printf("total %.1fms over %d queries\n", float64(sum.Microseconds())/1000, len(qs))
	return nil
}
