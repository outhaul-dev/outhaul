package core

// Limits is a container's resource ceiling. A zero field means no limit.
type Limits struct {
	MilliCPUs int64 // thousandths of a CPU; 0 = none
	Memory    int64 // bytes; 0 = none
	Pids      int64 // processes; 0 = none
}

// Or returns l with each zero field taken from fallback.
func (l Limits) Or(fallback Limits) Limits {
	if l.MilliCPUs == 0 {
		l.MilliCPUs = fallback.MilliCPUs
	}
	if l.Memory == 0 {
		l.Memory = fallback.Memory
	}
	if l.Pids == 0 {
		l.Pids = fallback.Pids
	}
	return l
}
