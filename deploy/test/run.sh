#!/usr/bin/env sh
# Runs every deploy/test/test_*.sh in its own shell, aggregates results.
set -u
here=$(cd "$(dirname "$0")" && pwd)
total=0 failed=0
for t in "$here"/test_*.sh; do
	[ -e "$t" ] || continue
	printf '\n== %s ==\n' "$(basename "$t")"
	out=$(TESTS_RUN=0 TESTS_FAIL=0 sh "$t"; echo "EXIT:$?")
	printf '%s\n' "$out" | grep -v '^EXIT:'
	code=$(printf '%s\n' "$out" | sed -n 's/^EXIT://p' | tail -1)
	[ "${code:-1}" -eq 0 ] || failed=$((failed+1))
	total=$((total+1))
done
printf '\n%d test file(s), %d failed\n' "$total" "$failed"
[ "$failed" -eq 0 ]
