package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// memMB returns total and currently available memory in MB.
func memMB() (total, avail int) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fs := strings.Fields(s.Text())
		if len(fs) < 2 {
			continue
		}
		kb, _ := strconv.Atoi(fs[1])
		switch fs[0] {
		case "MemTotal:":
			total = kb / 1024
		case "MemAvailable:":
			avail = kb / 1024
		}
	}
	return
}

// memPressure returns PSI "some avg10" for memory (0-100), or -1 if unavailable.
func memPressure() float64 {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				p, err := strconv.ParseFloat(v, 64)
				if err == nil {
					return p
				}
			}
		}
	}
	return -1
}
