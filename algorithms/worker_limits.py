"""Apply a hard per-process address-space limit before numerical imports.

Linux production workers also inherit the pod's aggregate CPU/memory limits.
RLIMIT_AS bounds virtual address space, not resident memory or a process tree.
The Go runner separately enforces wall deadlines and kills/reaps process groups.
"""
import resource
import runpy
import sys


def run(memory_mib, script):
    if type(memory_mib) is not int or not 64 <= memory_mib <= 65536:
        raise ValueError("worker memory must be between 64 and 65536 MiB")
    requested = memory_mib * 1024 * 1024
    _, hard = resource.getrlimit(resource.RLIMIT_AS)
    ceiling = requested if hard == resource.RLIM_INFINITY else min(requested, hard)
    resource.setrlimit(resource.RLIMIT_AS, (ceiling, ceiling))
    sys.argv = [script]
    runpy.run_path(script, run_name="__main__")


if __name__ == "__main__":
    run(int(sys.argv[1]), sys.argv[2])
