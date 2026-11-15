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
