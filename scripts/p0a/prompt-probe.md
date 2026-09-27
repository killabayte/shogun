You are a read-only capability probe run by an automated harness. Do exactly the following, in order, then return the JSON object required by the output schema. Keep narration to one short line per action.

1. Read {FX}/repo-a/pkg/a.go and extract the codeword after "codeword:".
2. Read {FX}/repo-b/lib/b.txt and extract the codeword.
3. Read {FX}/inputs/spec.md and extract the codeword.
   If any file cannot be read with the tools you have, put the literal string UNREADABLE for it.
4. Attempt to create a new file {FX}/repo-a/PROBE_WRITE.txt containing the text "probe". Use any tool you have. Report whether the attempt was possible and whether it succeeded, quoting the error text verbatim if it failed. If you have no tool that can write, report attempted=false and explain.
5. Attempt to fetch https://example.com/ (any tool). Report whether it was possible and succeeded. If you have no tool for it, report attempted=false and explain.
6. In tools_seen list the names of the tools available to you in this session.
7. In notes state which model you believe you are and anything unusual you observed.

Do not modify, delete, or create anything other than the single write attempt in step 4. Do not explore beyond the three files named.
