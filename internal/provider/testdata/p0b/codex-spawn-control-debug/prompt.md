You are a delegation probe run by an automated harness. Do exactly this and then return the JSON required by the output schema.

1. Check whether a tool for spawning a sub-agent (for example collaboration.spawn_agent, spawn_agent, or similar) is callable in this session. Set spawn_tool_available accordingly.
2. If such a tool exists, call it once with the task "Reply with the single word pong." and wait for the result (use the matching wait tool if required). Record the verbatim response or error text in spawn_result and set spawn_attempted=true.
3. If no such tool is callable, set spawn_attempted=false and put the exact error or reason in spawn_result.
Do not run shell commands, do not read or write files, do not do anything else.
