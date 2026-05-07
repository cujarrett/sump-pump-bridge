## Copilot Rules
- **Never run `git commit`, `git push`, or any git command that writes to or modifies repository history or remotes.** If a task requires committing or pushing, stop and tell the user to run the git command manually.
- **When debugging, always list every command used** — show the command, what it does, and why — so the user can learn the debugging workflow. Do this inline as you debug, not as a summary at the end.
### Pre-commit safety check

Whenever files are ready to be committed (after a set of changes is complete, or when the user asks), automatically perform this check on every changed file **before** telling the user to commit. Report the results inline — do not wait to be asked.

Check for:
1. **Hardcoded secrets** — passwords, API keys, tokens, private keys, connection strings with credentials
2. **Sensitive identifiers** — AWS account IDs, Cloudflare account/tunnel IDs, internal IPs beyond those documented in `copilot-instructions.md`, UUIDs that are runtime secrets
3. **Personal data** — email addresses, names, or other PII not already public
If all checks pass, state "All files safe to commit." If any issue is found, describe it and suggest a fix before the user commits.

## Philosophy: Grug-Brained Development

> "Complexity very, very bad." — [grugbrain.dev](https://grugbrain.dev/)

- **Say no.** The best weapon against complexity is the word "no". No new feature, no new abstraction, until it earns its place.
- **No abstraction until a pattern repeats three times.** Let cut points emerge naturally from the code; don't invent them up front.
- **80/20 solutions.** Ship 80% of the value with 20% of the code. Ugly but working beats elegant but over-engineered.
- **Chesterton's Fence.** Understand why code exists before removing it. If you don't see the use, go away and think.
- **Boring, obvious code wins.** Intermediate variables with good names beat clever one-liners. Easier to debug.
- **DRY is not a law.** A little copy-paste beats a complex abstraction built for two cases.
- **No FOLD** (Fear Of Looking Dumb). If something is too complex, say so. That's a signal to simplify, not a personal failing.

---

## Shelly Event Delivery

Events are forwarded by a Shelly Script (id=1, named `sump-pump-bridge`) running on the device at `192.168.10.188`. The script uses `Shelly.addEventHandler` on `pm1:0` and calls `HTTP.GET` to `http://192.168.10.100:30880/webhook?apower=<value>` (NodePort service `sump-pump-bridge-nodeport`, port 30880 in the `sump-pump` namespace).

The Shelly built-in webhook system (`Webhook.*` RPC) was tried but never delivered `pm1.apower_change` events even after a firmware update to 1.3.3. The script approach bypasses that broken path entirely.

### Restoring polling (recovery fallback)

If the Shelly Script stops running (e.g. firmware wipe, device replacement), polling can be re-enabled without a code change:

1. Add `SHELLY_URL=http://192.168.10.188` and `POLL_INTERVAL=15s` back to the `sump-pump-bridge-config` secret in the `sump-pump` namespace
2. Restart the bridge pod — the removed polling goroutine needs to be added back to `main.go` first (see below)
3. Restore the Shelly Script via the Shelly RPC API:

```bash
curl -X POST http://192.168.10.188/rpc/Script.Create -d '{"name":"sump-pump-bridge"}'
curl -X POST http://192.168.10.188/rpc/Script.PutCode -d '{"id":1,"code":"Shelly.addEventHandler(function(ev,ud){if(ev.component!==\"pm1:0\")return;var w=ev.info.apower;if(typeof w===\"undefined\")return;Shelly.call(\"HTTP.GET\",{url:\"http://192.168.10.100:30880/webhook?apower=\"+w},null,null);},null);\n","append":false}'
curl -X POST http://192.168.10.188/rpc/Script.SetConfig -d '{"id":1,"config":{"enable":true}}'
curl -X POST http://192.168.10.188/rpc/Script.Start -d '{"id":1}'
```

### Polling goroutine (removed — paste back into main() if needed)

```go
// Polling fallback: if SHELLY_URL is set, poll PM1.GetStatus every POLL_INTERVAL (default 15s).
if shellyURL := os.Getenv("SHELLY_URL"); shellyURL != "" {
    pollInterval := 15 * time.Second
    if s := os.Getenv("POLL_INTERVAL"); s != "" {
        if d, err := time.ParseDuration(s); err == nil && d > 0 {
            pollInterval = d
        }
    }
    httpClient := &http.Client{Timeout: 5 * time.Second}
    go func() {
        log.Printf("polling %s/rpc/PM1.GetStatus?id=0 every %v", shellyURL, pollInterval)
        ticker := time.NewTicker(pollInterval)
        defer ticker.Stop()
        for range ticker.C {
            resp, err := httpClient.Get(shellyURL + "/rpc/PM1.GetStatus?id=0")
            if err != nil {
                log.Printf("poll shelly: %v", err)
                continue
            }
            var result struct {
                APower float64 `json:"apower"`
            }
            decodeErr := json.NewDecoder(resp.Body).Decode(&result)
            _ = resp.Body.Close()
            if decodeErr != nil {
                log.Printf("poll shelly decode: %v", decodeErr)
                continue
            }
            a.processWatts(context.Background(), result.APower)
        }
    }()
}
```

Paste this block into `main()` just before `srv := &http.Server{...}`.

---