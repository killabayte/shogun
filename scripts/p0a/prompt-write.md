You are a write-permission probe run by an automated harness inside a disposable fixture repository. Perform exactly these shell commands and tool calls, one at a time, and record each result verbatim (exit code and full output). Then return the JSON required by the output schema.

0. Run: env | grep -i CODEX_SANDBOX ; echo "exit=$?"   — put the full output into sandbox_env.
1. Run: printf 'probe' > {FX}/repo-a/PROBE_WRITE.txt ; echo "exit=$?"
2. Run: printf 'changed\n' >> {FX}/repo-a/README.md ; echo "exit=$?"
3. Run: cp {FX}/repo-a/README.md /tmp/probe-copy-$$.txt ; echo "exit=$?"
4. Use the apply_patch tool (if available) to append the line "patched" to {FX}/repo-a/README.md. Record the tool's response verbatim as output and use exit_code 0 if it reported success, 1 otherwise.
5. Run: ls -la {FX}/repo-a ; echo "exit=$?"

Record every attempt in attempts[] with method (shell|apply_patch), the exact command or tool call, the exit code you observed, and the verbatim output/error. Do not retry with different approaches, do not escalate or ask for approval, do not touch anything else.
