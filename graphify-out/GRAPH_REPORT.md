# Graph Report - /Users/nd/projects/my/crypto/mm-standx  (2026-08-07)

## Corpus Check
- 102 files · ~97,335 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 1559 nodes · 3986 edges · 60 communities (54 shown, 6 thin omitted)
- Extraction: 87% EXTRACTED · 13% INFERRED · 0% AMBIGUOUS · INFERRED: 519 edges (avg confidence: 0.8)
- Token cost: 28,100 input · 8,760 output

## Community Hubs (Navigation)
- Configuration Validation
- Quote Evaluation
- Engine Test Fixtures
- Authentication Signing
- Configuration Loading
- Hourly Statistics
- Reward Scheduling
- Deployment and Strategy
- Money and Fill Events
- Quote Ladder
- Persistent Risk Guard
- WebSocket Event Handling
- Process Monitoring
- CLI Utilities
- Engine State Model
- WebSocket Connections
- Prometheus Metrics
- Uptime Tracking
- Session Storage
- Dependency Injection Runtime
- Gateway Routing
- Live Test Harness
- REST Client
- Risk Ledger Tests
- Domain Events
- Market Model Tests
- Metrics Interface
- Market Observation
- Position Sizing
- DI Providers
- StandX DTO Mapping
- Stream Recording
- Market Domain
- Adaptive Policy Tests
- StandX DTOs
- Trading Domain
- Account Polling
- Watchdog
- Market Sides
- Money Tests
- State Store
- Telemetry Collection
- Dry Run Gateway
- Shadow Fill Tests
- Exchange Reader
- Rate Limiting
- Dry Run Reader
- Live Smoke Tests
- Wallet Transfers
- Risk Execution Tests
- Shadow Engine
- Smoke Harness
- Mark Price Flow
- Actor Event Architecture
- Project Module

## God Nodes (most connected - your core abstractions)
1. `Bps` - 65 edges
2. `MustParsePrice()` - 55 edges
3. `Price` - 53 edges
4. `Qty` - 53 edges
5. `Clock` - 51 edges
6. `ClOrdID` - 50 edges
7. `Engine` - 46 edges
8. `MustParseQty()` - 42 edges
9. `Side` - 41 edges
10. `Bus` - 40 edges

## Surprising Connections (you probably didn't know these)
- `Paired Relaxed Appetite Configuration` --semantically_similar_to--> `Paired Strict Appetite Configuration`  [INFERRED] [semantically similar]
  config.ab-relaxed.yaml → config.ab-strict.yaml
- `Relaxed Eleven-Market Shadow Configuration` --semantically_similar_to--> `Strict Eleven-Market Shadow Configuration`  [INFERRED] [semantically similar]
  config.shadow-relaxed.yaml → config.shadow.yaml
- `TestSmoke()` --calls--> `PriceAtDistance()`  [INFERRED]
  test/live/smoke_test.go → internal/domain/market/market.go
- `TestSmoke()` --calls--> `Notional()`  [INFERRED]
  test/live/smoke_test.go → internal/domain/money/money.go
- `Decimal Money Representation` --conceptually_related_to--> `Persistent Risk Circuit`  [INFERRED]
  AGENTS.md → PLAN.md

## Import Cycles
- None detected.

## Hyperedges (group relationships)
- **CI Build Publish Deploy Chain** — _github_workflows_ci_cd_ci_job, _github_workflows_ci_cd_container_job, _github_workflows_ci_cd_publish_job, _github_workflows_ci_cd_deploy_job [EXTRACTED 1.00]
- **Strict and Relaxed Paired Appetite Experiment** — config_ab_strict_strict_treatment, config_ab_relaxed_relaxed_treatment, docs_ab_appetite_2026_08_05_paired_experiment [INFERRED 0.95]
- **Order Safety and Recovery Control Loop** — plan_safe_distance_policy, plan_immediate_fill_flattening, plan_risk_circuit, plan_reconciliation, plan_dead_mans_switch [INFERRED 0.85]

## Communities (60 total, 6 thin omitted)

### Community 0 - "Configuration Validation"
Cohesion: 0.06
Nodes (72): entry, message, series, RawMessage, Time, main(), observeBook(), observeMark() (+64 more)

### Community 1 - "Quote Evaluation"
Cohesion: 0.05
Nodes (41): Action, Appetite, Context, Engine, Quote, Time, priorityOf(), Quote (+33 more)

### Community 2 - "Engine Test Fixtures"
Cohesion: 0.06
Nodes (46): recordingGateway, stubTicker, testClock, Context, Context, Context, Client, Context (+38 more)

### Community 3 - "Authentication Signing"
Cohesion: 0.05
Nodes (48): Address, Authenticator, loginResponse, prepareSignInResponse, Session, signInClaims, Wallet, T (+40 more)

### Community 4 - "Configuration Loading"
Cohesion: 0.07
Nodes (60): Fees, Mode, SymbolRisk, UptimeTier, envKeyResolver(), Appetite, Appetite, Config (+52 more)

### Community 5 - "Hourly Statistics"
Cohesion: 0.08
Nodes (35): New(), CancelFunc, Context, Duration, Logger, Time, New(), seriesKey() (+27 more)

### Community 6 - "Reward Scheduling"
Cohesion: 0.08
Nodes (43): OffHours, Program, Decimal, Time, percentile(), Program, T, liveCurve() (+35 more)

### Community 7 - "Deployment and Strategy"
Cohesion: 0.05
Nodes (52): Test and Build Job, Container Build Job, Vultr Deployment Job, Docker Hub Publish Job, CI/CD Workflow, Decimal Money Representation, Hexagonal Architecture, Layered Configuration Policy (+44 more)

### Community 8 - "Money and Fill Events"
Cohesion: 0.06
Nodes (12): QuoteFilled, Decimal, NewPrice(), NewQty(), Notional(), ParsePrice(), ParseQty(), QtyForNotional() (+4 more)

### Community 9 - "Quote Ladder"
Cohesion: 0.12
Nodes (33): Duration, Time, New(), atDistance(), desired(), fullLadder(), Ladder, T (+25 more)

### Community 10 - "Persistent Risk Guard"
Cohesion: 0.10
Nodes (22): boolAsFloat(), CancelFunc, Context, Duration, Logger, Risk, Time, New() (+14 more)

### Community 11 - "WebSocket Event Handling"
Cohesion: 0.13
Nodes (27): E, alpha, beta, Event, PanicHandler, subscription, Context, RawMessage (+19 more)

### Community 12 - "Process Monitoring"
Cohesion: 0.08
Nodes (27): Client, Config, Context, Duration, Logger, main(), run(), watch() (+19 more)

### Community 13 - "CLI Utilities"
Cohesion: 0.10
Nodes (28): System, systemTicker, cmdLatency(), Client, Context, Duration, percentile(), cmdAuth() (+20 more)

### Community 14 - "Engine State Model"
Cohesion: 0.12
Nodes (14): Params, Appetite, CancelFunc, Config, Context, Decimal, Engine, Health (+6 more)

### Community 15 - "WebSocket Connections"
Cohesion: 0.10
Nodes (18): Conn, Context, Logger, NewServer(), CancelFunc, Context, Duration, Exchange (+10 more)

### Community 16 - "Prometheus Metrics"
Cohesion: 0.10
Nodes (9): CounterVec, Gauge, GaugeVec, HistogramVec, boolValue(), Symbol, provideMetrics(), Prometheus (+1 more)

### Community 17 - "Uptime Tracking"
Cohesion: 0.17
Nodes (23): Decimal, Duration, Program, Time, New(), fill(), Duration, Program (+15 more)

### Community 18 - "Session Storage"
Cohesion: 0.15
Nodes (21): CachedSession, FileStore, NopStore, equalFold(), Duration, Time, lower(), NewFileStore() (+13 more)

### Community 19 - "Dependency Injection Runtime"
Cohesion: 0.18
Nodes (26): Bus, buildProgram(), Client, Collector, Config, Lifecycle, Logger, Program (+18 more)

### Community 20 - "Gateway Routing"
Cohesion: 0.23
Nodes (22): FastGateway, Router, stubGateway, Logger, New(), Context, T, newRouter() (+14 more)

### Community 21 - "Live Test Harness"
Cohesion: 0.13
Nodes (13): App, configPath(), Client, Config, Context, harness, Logger, Mutex (+5 more)

### Community 22 - "REST Client"
Cohesion: 0.14
Nodes (19): backoff(), Context, Duration, Exchange, Logger, Client, New(), parseError() (+11 more)

### Community 23 - "Risk Ledger Tests"
Cohesion: 0.30
Nodes (24): New(), at(), Decimal, T, Time, limits(), TestCostModelSubstitutesForASilentTradeChannel(), TestDailyLossTripsTheSwitch() (+16 more)

### Community 24 - "Domain Events"
Cohesion: 0.11
Nodes (13): BalanceUpdated, KillSwitchTripped, MarkPriceStale, OpenOrdersSnapshot, OrderRejected, OrderUpdated, PositionUpdated, StreamConnected (+5 more)

### Community 25 - "Market Model Tests"
Cohesion: 0.23
Nodes (23): DistanceBps(), NewBook(), PriceAtDistance(), T, Time, spec(), TestDepthNotionalCountsOnlyWhatIsInsideTheBand(), TestDepthNotionalIsZeroWithoutAReference() (+15 more)

### Community 27 - "Market Observation"
Cohesion: 0.16
Nodes (11): BookUpdated, CancelFunc, Context, Duration, Logger, Mutex, Symbol, New() (+3 more)

### Community 28 - "Position Sizing"
Cohesion: 0.22
Nodes (21): Compute(), Decimal, Program, btcProgram(), btcSpec(), Program, T, inputs() (+13 more)

### Community 29 - "DI Providers"
Cohesion: 0.22
Nodes (21): SessionStore, SessionID, Client, Config, Logger, provideClock(), provideDryRunGateway(), provideEventBus() (+13 more)

### Community 30 - "StandX DTO Mapping"
Cohesion: 0.22
Nodes (19): DepthBook, SymbolInfo, SymbolPrice, WalletBalance, Balance(), Book(), Decimal, Time (+11 more)

### Community 31 - "Stream Recording"
Cohesion: 0.17
Nodes (12): Bool, Context, Exchange, Logger, Mutex, Symbol, handler, New() (+4 more)

### Community 32 - "Market Domain"
Cohesion: 0.18
Nodes (7): Decimal, Time, Book, Level, MarkPrice, Symbol, SymbolSpec

### Community 33 - "Adaptive Policy Tests"
Cohesion: 0.32
Nodes (16): appetite(), bounds(), Appetite, T, TestChosenDistanceAlwaysCoversTheRisk(), TestDangerThresholdsSurviveAWideDeadBand(), TestDeadBandScalesWithVolatility(), TestDerivedThresholdsAreAlwaysOrdered() (+8 more)

### Community 34 - "StandX DTOs"
Cohesion: 0.14
Nodes (13): Balance, BatchEnvelope, CancelOrderRequest, CancelOrdersRequest, Envelope, MarketOverview, MarketSummary, NewOrderRequest (+5 more)

### Community 35 - "Trading Domain"
Cohesion: 0.23
Nodes (12): Decimal, Symbol, Time, Balance, MarginMode, Order, OrderID, OrderStatus (+4 more)

### Community 36 - "Account Polling"
Cohesion: 0.29
Nodes (7): Poller, CancelFunc, Context, Duration, Logger, Symbol, New()

### Community 37 - "Watchdog"
Cohesion: 0.24
Nodes (9): CancelFunc, Context, Duration, Logger, Mutex, Symbol, Time, New() (+1 more)

### Community 39 - "Money Tests"
Cohesion: 0.28
Nodes (12): T, TestBpsRatio(), TestComparisons(), TestJSONRoundTrip(), TestNotional(), TestParsePreservesPrecision(), TestParseRejectsGarbage(), TestQtyForNotional() (+4 more)

### Community 40 - "State Store"
Cohesion: 0.27
Nodes (7): NewFile(), T, TestCorruptSnapshotIsAnError(), TestRoundTrip(), TestSaveLeavesNoPartialFile(), FileStore, snapshot

### Community 41 - "Telemetry Collection"
Cohesion: 0.30
Nodes (7): CancelFunc, Context, Duration, Logger, New(), Metrics, Collector

### Community 42 - "Dry Run Gateway"
Cohesion: 0.25
Nodes (9): Gateway, Deps, Logger, Mutex, New(), NewReader(), Logger, provideExchangeReader() (+1 more)

### Community 43 - "Shadow Fill Tests"
Cohesion: 0.47
Nodes (10): countingMetrics, book(), Engine, T, shadowEngine(), TestLiveModeDoesNotGuess(), TestOneTouchIsCountedOnce(), TestQuoteIsCountedWhenTheMarketReachesIt() (+2 more)

### Community 44 - "Exchange Reader"
Cohesion: 0.40
Nodes (3): Context, Client, Symbol

### Community 45 - "Rate Limiting"
Cohesion: 0.33
Nodes (5): Context, Duration, RateLimit, New(), Limiter

### Community 46 - "Dry Run Reader"
Cohesion: 0.50
Nodes (3): Reader, Context, Symbol

### Community 47 - "Live Smoke Tests"
Cohesion: 0.47
Nodes (4): Symbol, Order(), isCanceled(), isOpen()

### Community 48 - "Wallet Transfers"
Cohesion: 0.73
Nodes (3): Context, Decimal, Client

### Community 49 - "Risk Execution Tests"
Cohesion: 0.73
Nodes (5): execution(), T, TestEpisodeOfElevenOhNine(), TestEpisodeOfTwelveTwentyNine(), TestReportedFeeWins()

### Community 51 - "Smoke Harness"
Cohesion: 0.50
Nodes (3): harness, T, TestSmoke()

## Knowledge Gaps
- **34 isolated node(s):** `message`, `github.com/nd/mm-standx`, `prepareSignInResponse`, `loginResponse`, `Order` (+29 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **6 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `Clock` connect `Dependency Injection Runtime` to `Engine Test Fixtures`, `Authentication Signing`, `Account Polling`, `Hourly Statistics`, `Watchdog`, `Telemetry Collection`, `Dry Run Gateway`, `Persistent Risk Guard`, `Process Monitoring`, `CLI Utilities`, `Rate Limiting`, `WebSocket Connections`, `REST Client`, `Market Observation`, `DI Providers`, `Stream Recording`?**
  _High betweenness centrality (0.142) - this node is a cross-community bridge._
- **Why does `Bps` connect `Quote Evaluation` to `Configuration Validation`, `Adaptive Policy Tests`, `Market Domain`, `Configuration Loading`, `Reward Scheduling`, `Money Tests`, `Money and Fill Events`, `Quote Ladder`, `Shadow Engine`, `Market Model Tests`, `Market Observation`, `Position Sizing`?**
  _High betweenness centrality (0.086) - this node is a cross-community bridge._
- **Why does `ClOrdID` connect `Engine Test Fixtures` to `Configuration Validation`, `Quote Evaluation`, `Trading Domain`, `Quote Ladder`, `Dry Run Gateway`, `Shadow Fill Tests`, `Engine State Model`, `Live Smoke Tests`, `Gateway Routing`, `Live Test Harness`, `Domain Events`?**
  _High betweenness centrality (0.073) - this node is a cross-community bridge._
- **Are the 15 inferred relationships involving `Bps` (e.g. with `TestDistanceRoundTrip()` and `TestSnapPriceNeverReducesDistance()`) actually correct?**
  _`Bps` has 15 INFERRED edges - model-reasoned connections that need verification._
- **Are the 52 inferred relationships involving `MustParsePrice()` (e.g. with `dangerEngine()` and `liveRung()`) actually correct?**
  _`MustParsePrice()` has 52 INFERRED edges - model-reasoned connections that need verification._
- **What connects `message`, `github.com/nd/mm-standx`, `prepareSignInResponse` to the rest of the system?**
  _34 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `Configuration Validation` be split into smaller, more focused modules?**
  _Cohesion score 0.05811027224891329 - nodes in this community are weakly interconnected._