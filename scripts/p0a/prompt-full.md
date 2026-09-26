You are a capability probe run by an automated harness inside disposable fixture repositories. Do exactly the following, in order, recording results verbatim, then return the JSON required by the output schema. One short narration line per step.

1. Run: cat {FX}/repo-a/pkg/a.go    — extract the codeword after "codeword:".
2. Run: cat {FX}/repo-b/lib/b.txt   — extract the codeword.
3. Run: cat {FX}/inputs/spec.md     — extract the codeword. Use UNREADABLE if a file cannot be read.
4. Run: printf 'probe' > {FX}/repo-a/PROBE_WRITE.txt ; echo "exit=$?"          — record in write_attempts (method shell).
5. Run: cp {FX}/repo-a/README.md {TMPP} ; echo "exit=$?"                        — record in write_attempts (method shell).
6. Use the apply_patch tool to append the line "patched" to {FX}/repo-a/README.md; record the verbatim tool response (method apply_patch, exit_code 0 if it reported success, 1 otherwise).
7. Run: curl --max-time 15 -sS https://example.com/ ; echo "exit=$?"            — record in network_attempt.
8. If a sub-agent spawning tool (collaboration.spawn_agent / spawn_agent) is callable, call it once with the task "Reply with the single word pong." and wait for it; record the verbatim response or error in spawn_attempt. If it is not callable, record attempted=false with the reason.
9. List the tool names available to you in tools_seen. In notes: which model you believe you are, one sentence.

Do not retry with alternative approaches, do not ask for approval, do not touch anything else.
