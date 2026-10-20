package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type cgroupMemoryMount struct {
	root      string
	directory string
	v2        bool
}

func memoryCgroupMounts() []cgroupMemoryMount {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err == nil {
		mounts := []cgroupMemoryMount{}
		unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
		for _, line := range strings.Split(string(data), "\n") {
			before, after, found := strings.Cut(line, " - ")
			fields, filesystem := strings.Fields(before), strings.Fields(after)
			if !found || len(fields) < 5 || len(filesystem) < 3 {
				continue
			}
			v2 := filesystem[0] == "cgroup2"
			if !v2 && (filesystem[0] != "cgroup" || !strings.Contains(","+filesystem[2]+",", ",memory,")) {
				continue
			}
			mounts = append(mounts, cgroupMemoryMount{
				root: filepath.Clean(unescape.Replace(fields[3])), directory: filepath.Clean(unescape.Replace(fields[4])), v2: v2,
			})
		}
		if len(mounts) > 0 {
			return mounts
		}
	}
	return []cgroupMemoryMount{
		{root: "/", directory: "/sys/fs/cgroup", v2: true},
		{root: "/", directory: "/sys/fs/cgroup/memory"},
	}
}

// goMemoryLimitSetting applies the consolidated resource switch. off removes
// the soft limit entirely; auto honors an explicit standard GOMEMLIMIT and
// otherwise derives a budget from the detected cgroup/host memory.
func goMemoryLimitSetting(resourceMode string, memoryLimit, available int64) (int64, string) {
	if resourceMode == "off" {
		return math.MaxInt64, "off"
	}
	if standard := strings.TrimSpace(os.Getenv("GOMEMLIMIT")); standard != "" {
		return configuredGoMemoryLimit("GOMEMLIMIT", standard), "GOMEMLIMIT"
	}
	return automaticGoMemoryLimit(memoryLimit, available), "auto"
}

func configuredGoMemoryLimit(name, raw string) int64 {
	if raw == "off" {
		return math.MaxInt64
	}
	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"B", 1},
	} {
		if strings.HasSuffix(raw, unit.suffix) {
			raw = strings.TrimSuffix(raw, unit.suffix)
			multiplier = unit.bytes
			break
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 || value == 0 || value > math.MaxInt64/multiplier {
		panic(fmt.Sprintf("invalid %s: use off or a byte count with optional B/KiB/MiB/GiB/TiB suffix", name))
	}
	return value * multiplier
}

// Reserve 20% of the smaller of the effective RAM limit and startup available
// RAM, with at least 32 MiB of headroom when the budget permits. This covers
// binary mappings, SQLite mmap allocations, kernel/page-cache charges and
// other processes; swap must not inflate the physical-memory budget.
func automaticGoMemoryLimit(memoryLimit, available int64) int64 {
	budget := memoryLimit
	if available >= 0 && (budget <= 0 || available < budget) {
		budget = available
	}
	if budget < 0 || budget == 0 && available < 0 {
		// Detection unavailable: preserve the runtime's existing setting.
		return -1
	}
	reserve := min(max(budget/5, 32<<20), budget/2)
	limit := budget - reserve
	if limit >= 1<<20 {
		limit = limit / (1 << 20) * (1 << 20)
	}
	return limit
}
