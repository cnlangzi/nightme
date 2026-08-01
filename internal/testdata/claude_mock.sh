#!/bin/sh
# Claude Code bridge mock — sh wrapper that ignores the bridge's
# argv (--print, --input-format stream-json, ...) and runs the
# Python implementation. The args are essential for the real
# claude binary but meaningless for our mock.
exec python3 "$(dirname "$0")/claude_mock.py"
