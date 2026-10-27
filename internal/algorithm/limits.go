package algorithm

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// ValidateMemoryLimit validates the optional per-worker virtual memory ceiling.
// Zero is supported for injected legacy runners. Production defaults to 1024 MiB.
func ValidateMemoryLimit(mib int) error {
	if mib != 0 && (mib < 64 || mib > 65536) {
		return fmt.Errorf("worker memory must be zero or between 64 and 65536 MiB")
	}
	return nil
}

func workerCommand(command command, wd, script string, memoryMiB int) (*exec.Cmd, error) {
	if err := ValidateMemoryLimit(memoryMiB); err != nil {
		return nil, err
	}
	args := []string{path.Join(wd, script)}
	if memoryMiB != 0 {
		args = []string{path.Join(wd, "algorithms/worker_limits.py"), strconv.Itoa(memoryMiB), args[0]}
	}
	cmd := command(entrypoint, args...)
	// Apply inside the runner as well as the chart; inherited host BLAS settings
	// must not multiply model concurrency into unrestricted thread pools.
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	keys := []string{"OPENBLAS_NUM_THREADS", "OMP_NUM_THREADS", "MKL_NUM_THREADS", "NUMEXPR_NUM_THREADS", "VECLIB_MAXIMUM_THREADS", "BLIS_NUM_THREADS", "PYTHONDONTWRITEBYTECODE"}
	filtered := make([]string, 0, len(env)+len(keys))
	for _, value := range env {
		keep := true
		for _, key := range keys {
			if strings.HasPrefix(value, key+"=") {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, value)
		}
	}
	for _, key := range keys {
		filtered = append(filtered, key+"=1")
	}
	cmd.Env = filtered
	return cmd, nil
}
