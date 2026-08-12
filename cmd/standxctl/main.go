// Command standxctl — диагностическая утилита для проверки доступа к StandX.
//
// Служит для подготовки счёта к работе бота: проверяет авторизацию, показывает
// состояние кошельков и переводит средства между Cash Wallet и Perps Wallet.
// Ордера не отправляет.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/shopspring/decimal"
	"go.uber.org/fx"

	"github.com/nd/mm-standx/internal/adapters/standx/rest"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/di"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/ports"
)

const commandTimeout = 30 * time.Second

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to the configuration file")
	symbol := flag.String("symbol", "", "symbol to filter the orders and positions commands")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	if err := run(*configPath, flag.Arg(0), flag.Args()[1:], *symbol); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `standxctl - inspect StandX account access

Usage:
  standxctl [flags] <command> [arguments]

Commands:
  auth                      verify authentication and show session state
  balance                   show Perps Wallet balance
  positions                 show open positions
  orders                    show resting orders
  symbols                   show markets and trading specifications
  latency                   measure round-trip time and verify keep-alive
  to-perps <amount>         move DUSD from Cash Wallet to Perps Wallet
  to-spot  <amount>         move DUSD from Perps Wallet to Cash Wallet

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
The private key is read from STANDX_WALLET_PK or from a .env file.
`)
}

func run(configPath, command string, args []string, symbolFlag string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration:\n%w", err)
	}
	// Утилита ничего не записывает: каталог для рыночных данных ей не нужен.
	cfg.Runtime.RecordingDir = ""

	var (
		client     *rest.Client
		tokens     ports.TokenProvider
		httpClient *http.Client
		log        *slog.Logger
	)
	app := fx.New(
		fx.Supply(cfg),
		fx.NopLogger,
		di.Core,
		di.StandX,
		fx.Populate(&client, &tokens, &httpClient, &log),
	)
	if err := app.Err(); err != nil {
		return fmt.Errorf("failed to build dependencies: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	switch command {
	case "auth":
		return cmdAuth(ctx, cfg, tokens)
	case "balance":
		return cmdBalance(ctx, client)
	case "positions":
		return cmdPositions(ctx, client, market.Symbol(symbolFlag))
	case "orders":
		return cmdOrders(ctx, client, market.Symbol(symbolFlag))
	case "symbols":
		return cmdSymbols(ctx, client)
	case "latency":
		return cmdLatency(ctx, httpClient, cfg.Exchange.RESTBaseURL, 20)
	case "to-perps", "to-spot":
		return cmdTransfer(ctx, client, command, args)
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func cmdAuth(ctx context.Context, cfg *config.Config, tokens ports.TokenProvider) error {
	fmt.Printf("chain:         %s\n", cfg.Exchange.Chain)
	fmt.Printf("session file:  %s\n", orNone(cfg.Runtime.SessionPath))
	fmt.Printf("wallet key:    %s\n", present(!cfg.Exchange.WalletPrivateKey.IsEmpty()))

	started := time.Now()
	token, err := tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	fmt.Printf("\nauthentication: succeeded in %s\n", time.Since(started).Round(time.Millisecond))
	// Сам токен не печатаем: он даёт полный торговый доступ к счёту.
	fmt.Printf("token:          obtained, %d characters\n", len(token))
	return nil
}

func cmdBalance(ctx context.Context, client *rest.Client) error {
	balance, err := client.Balance(ctx)
	if err != nil {
		return err
	}

	w := newTable()
	fmt.Fprintf(w, "equity\t%s DUSD\ttotal funds including unrealized PnL\n", balance.Equity)
	fmt.Fprintf(w, "available\t%s DUSD\tfree for new orders\n", balance.Available)
	fmt.Fprintf(w, "locked\t%s DUSD\theld by resting orders\n", balance.Locked)
	fmt.Fprintf(w, "margin\t%s DUSD\tbacking open positions\n", balance.Margin)
	if err := w.Flush(); err != nil {
		return err
	}

	if balance.Equity.IsZero() {
		fmt.Println("\nPerps Wallet is empty. A deposit that has already arrived sits in the")
		fmt.Println("Cash Wallet; move it with: standxctl to-perps <amount>")
	}
	return nil
}

func cmdPositions(ctx context.Context, client *rest.Client, symbol market.Symbol) error {
	positions, err := client.Positions(ctx, symbol)
	if err != nil {
		return err
	}
	if len(positions) == 0 {
		fmt.Println("no open positions")
		return nil
	}

	w := newTable()
	fmt.Fprintln(w, "SYMBOL\tQTY\tENTRY\tMARK\tLIQUIDATION\tUPNL\tLEVERAGE")
	for _, p := range positions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%dx\n",
			p.Symbol, p.Qty, p.EntryPrice, p.MarkPrice, p.LiqPrice, p.Unrealized, p.Leverage)
	}
	return w.Flush()
}

func cmdOrders(ctx context.Context, client *rest.Client, symbol market.Symbol) error {
	orders, err := client.OpenOrders(ctx, symbol)
	if err != nil {
		return err
	}
	if len(orders) == 0 {
		fmt.Println("no resting orders")
		return nil
	}

	w := newTable()
	fmt.Fprintln(w, "SYMBOL\tSIDE\tPRICE\tQTY\tFILLED\tTIF\tSTATUS\tCL_ORD_ID")
	for _, o := range orders {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			o.Symbol, o.Side, o.Price, o.Qty, o.FilledQty, o.TimeInForce, o.Status, o.ClOrdID)
	}
	return w.Flush()
}

// cmdSymbols печатает рынки вместе со спецификациями.
//
// Это входные данные для расчёта размеров: тик цены определяет достижимую
// точность дистанции, а минимальный размер — нижнюю границу капитала,
// при которой котирование вообще возможно.
func cmdSymbols(ctx context.Context, client *rest.Client) error {
	overview, err := client.MarketOverview(ctx)
	if err != nil {
		return err
	}

	w := newTable()
	fmt.Fprintln(w, "SYMBOL\tMARK\tPRICE TICK\tQTY TICK\tMIN QTY\tMAX ORDERS\tMAKER\tTAKER")
	for _, s := range overview.Symbols {
		symbol := market.Symbol(s.Symbol)
		spec, specErr := client.SymbolSpec(ctx, symbol)
		if specErr != nil {
			fmt.Fprintf(w, "%s\t%s\terror: %v\n", s.Symbol, s.MarkPrice, specErr)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%d\t%s\t%s\n",
			s.Symbol, s.MarkPrice,
			spec.PriceTickDecimals, spec.QtyTickDecimals,
			spec.MinOrderQty, spec.MaxOpenOrders,
			spec.MakerFee, spec.TakerFee)
	}
	return w.Flush()
}

func cmdTransfer(ctx context.Context, client *rest.Client, command string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("provide an amount, for example: standxctl %s 100", command)
	}
	amount, err := decimal.NewFromString(args[0])
	if err != nil {
		return fmt.Errorf("amount %q is not a number: %w", args[0], err)
	}

	if command == "to-perps" {
		if err := client.TransferToPerps(ctx, amount); err != nil {
			return err
		}
		fmt.Printf("moved %s DUSD to Perps Wallet\n", amount)
	} else {
		if err := client.TransferToSpot(ctx, amount); err != nil {
			return err
		}
		fmt.Printf("moved %s DUSD to Cash Wallet\n", amount)
	}

	fmt.Println("verify with: standxctl balance")
	return nil
}

func newTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

func present(ok bool) string {
	if ok {
		return "set"
	}
	return "not set"
}

func orNone(s string) string {
	if s == "" {
		return "disabled"
	}
	return s
}
