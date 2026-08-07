---
type: "query"
date: "2026-08-07T21:50:36.290079+00:00"
question: "Fix the market-maker bot risks and determine whether 10000 DUSD is required for profit"
contributor: "graphify"
outcome: "useful"
source_nodes: ["Tracker", "Gateway", "Risk", "Uptime"]
---

# Q: Fix the market-maker bot risks and determine whether 10000 DUSD is required for profit

## Answer

Expanded from the project graph vocabulary through uptime, tracker, tier, reward, economics, risk, fill, burst, cooldown, gateway, cancel, and batch. Source inspection confirmed that mid-hour uptime was optimistic, panic cancellation was sequential at the engine and WS fallback layers, and the symbol actor waited for single-command ACKs. Implemented a conservative UTC sample grid, economics-aware uptime tiers, live scaling gate, transient fill-burst cooldown, one-batch panic cancellation, real WS pipelining, and send-and-return single WS commands. Unit economics remain linear in notional, so no 10000 DUSD profitability threshold exists; reward USD per Maker Hour must exceed fill cost regardless of balance.

## Outcome

- Signal: useful

## Source Nodes

- Tracker
- Gateway
- Risk
- Uptime