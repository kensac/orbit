package runner

const (
	minAvailableMemory    = uint64(1 << 30)
	hostPressurePercent   = uint64(10)
	cgroupPressurePercent = uint64(85)
)

func memoryPressure(current, limit, available, total uint64, cgroup bool) bool {
	if cgroup && limit > 0 {
		return current >= limit/100*cgroupPressurePercent
	}
	if total == 0 {
		return false
	}
	minimum := max(minAvailableMemory, total/100*hostPressurePercent)
	return available <= minimum
}
