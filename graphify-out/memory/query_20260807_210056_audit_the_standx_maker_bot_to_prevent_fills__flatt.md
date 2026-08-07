---
type: "query"
date: "2026-08-07T21:00:56.866476+00:00"
question: "Audit the StandX maker bot to prevent fills, flatten immediately, and recover safely after volatility spikes"
contributor: "graphify"
outcome: "useful"
source_nodes: ["Ladder", ".Danger()", ".onFill()", ".flatten()", "Long-Lived Volatility Memory", "Exchange State Reconciliation"]
---

# Q: Audit the StandX maker bot to prevent fills, flatten immediately, and recover safely after volatility spikes

## Answer

Expanded from original query via graph vocab: [quote, fill, flatten, risk, volatility, memory, ladder, distance, safe, order, reconciliation, uptime]. The control loop crosses the adaptive distance classifier, quote ladder Danger/Plan paths, engine onFill/flatten lifecycle, bounded volatility estimator, uptime tracker, persistent risk guard, and exchange reconciliation. The relevant implementation points are internal/domain/quotes/ladder.go, internal/app/engine/evaluate.go, internal/domain/volatility/estimator.go, internal/domain/uptime/tracker.go, and internal/app/engine/reconcile.go.

## Outcome

- Signal: useful

## Source Nodes

- Ladder
- .Danger()
- .onFill()
- .flatten()
- Long-Lived Volatility Memory
- Exchange State Reconciliation