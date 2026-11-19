package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// memMB returns total and currently available memory in MB.
// Available = free + inactive + speculative + purgeable pages, the closest
// macOS has to Linux MemAvailable.
func memMB() (total, avail int) {
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		b, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		total = int(b >> 20)
	}
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return total, 0
	}
	pageSize, pages := int64(4096), int64(0)
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 {
			if j := strings.Index(line, "page size of "); j >= 0 {
				fmt.Sscanf(line[j:], "page size of %d", &pageSize)
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			n, _ := strconv.ParseInt(strings.Trim(v, " ."), 10, 64)
			pages += n
		}
	}
	return total, int(pages * pageSize >> 20)
}

// memPressure: macOS has no PSI; -1 turns the gate off.
func memPressure() float64 { return -1 }
