package rest

import (
	"context"
	"fmt"
	"net/http"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/adapters/standx/dto"
	"github.com/nd/mm-standx/internal/ports"
)

// defaultToken — расчётный актив StandX.
const defaultToken = "DUSD"

// TransferToPerps переводит средства из Cash Wallet в Perps Wallet.
//
// Депозит приходит в Cash Wallet, а торговля идёт с Perps Wallet, поэтому без
// этого перевода пополненный счёт выглядит пустым: query_balance показывает
// нули, и любой ордер отклоняется по нехватке маржи.
func (c *Client) TransferToPerps(ctx context.Context, amount decimal.Decimal) error {
	return c.transfer(ctx, "/api/transfer_to_perps", amount)
}

// TransferToSpot возвращает средства из Perps Wallet в Cash Wallet.
// Требуется перед выводом: вывод идёт только из Cash Wallet.
func (c *Client) TransferToSpot(ctx context.Context, amount decimal.Decimal) error {
	return c.transfer(ctx, "/api/transfer_to_spot", amount)
}

func (c *Client) transfer(ctx context.Context, path string, amount decimal.Decimal) error {
	if !amount.IsPositive() {
		return fmt.Errorf("transfer: amount must be positive, got %s", amount)
	}

	var envelope dto.Envelope
	err := c.do(ctx, request{
		method: http.MethodPost,
		path:   path,
		body: map[string]string{
			"amount": amount.String(),
			"token":  defaultToken,
		},
		auth:     true,
		signed:   true,
		priority: ports.PriorityBackground,
	}, &envelope)
	if err != nil {
		return fmt.Errorf("transfer %s: %w", path, err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("transfer %s: code %d: %s", path, envelope.Code, envelope.Message)
	}
	return nil
}
