#!/bin/bash
# Collect the syscalls CPython actually makes, across representative
# workloads, so the seccomp allowlist is built from evidence.
#
# Runs each workload under strace inside the profiling image and prints the
# union of observed syscall names, one per line.
set -euo pipefail

IMAGE=${IMAGE:-sandbox-strace}

# Workloads chosen to cover what the sandbox must support: interpreter
# startup, imports, file IO in /tmp, threads, subprocess/fork, exceptions,
# and large allocations.
WORKLOADS=(
  'print("hello")'
  'import json,math,random,re,datetime; print(json.dumps({"x":math.pi}))'
  'open("/tmp/f","w").write("x"*10000); print(open("/tmp/f").read()[:5])'
  'import threading; t=threading.Thread(target=lambda: None); t.start(); t.join(); print("threads")'
  'import os
pid=os.fork()
if pid==0: os._exit(0)
os.waitpid(pid,0); print("fork")'
  'raise SystemExit(3)'
  'x=bytearray(64*1024*1024); print(len(x))'
  'import time; time.sleep(0.1); print("slept")'
  'import subprocess; print(subprocess.run(["echo","hi"],capture_output=True).stdout)'
)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for i in "${!WORKLOADS[@]}"; do
  code=${WORKLOADS[$i]}
  docker run --rm \
    --security-opt seccomp=unconfined \
    --cap-add=SYS_PTRACE \
    "$IMAGE" \
    strace -f -qq -c -U name -- python3 -u -c "$code" \
    2>"$tmp/err.$i" >/dev/null || true
  # strace -c writes its summary table to stderr; the first column is the
  # syscall name once the header and separator rows are dropped.
  awk 'NR>2 && $1 !~ /^-+$/ && $1 != "total" {print $1}' "$tmp/err.$i" || true
done | sort -u | grep -E '^[a-z_0-9]+$'
