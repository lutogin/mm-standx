package mapper

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/adapters/standx/dto"
)

// Баланс — вход в расчёт размера, и с 15 августа единственный.
//
// Настроенной аллокации больше нет: нотионал считается как equity ×
// utilizationPct ÷ доля блокировки. Ошибка в разборе этих трёх полей теперь
// не «неточная сводка», а неверный размер каждой котировки, поэтому разбор
// закреплён на форме, которую биржа действительно присылает.

// Ответ query_balance живого счёта, 14 августа: 11091.87 DUSD на счёте,
// 10275.90 свободных, 815.97 под висящими ордерами.
const liveBalance = `{
  "isolated_balance": "0",
  "isolated_upnl": "0",
  "cross_balance": "11091.87",
  "cross_margin": "815.97",
  "cross_upnl": "0",
  "locked": "815.97",
  "cross_available": "10275.90",
  "balance": "11091.87",
  "upnl": "0",
  "equity": "11091.87",
  "pnl_freeze": "0"
}`

func TestBalanceCarriesTheThreeFieldsSizingDependsOn(t *testing.T) {
	var d dto.Balance
	require.NoError(t, json.Unmarshal([]byte(liveBalance), &d))

	b := Balance(d)
	require.Equal(t, "11091.87", b.Equity.String(), "equity is what the budget is a share of")
	require.Equal(t, "10275.9", b.Available.String())
	require.Equal(t, "815.97", b.Locked.String(), "locked is the numerator of the measured margin rate")
}

// equity, а не available: свободный остаток уже уменьшен нашими же ордерами,
// и бюджет от него — петля, сходящаяся примерно на 40% капитала.
func TestEquityIsNotTheFreeBalance(t *testing.T) {
	var d dto.Balance
	require.NoError(t, json.Unmarshal([]byte(liveBalance), &d))

	b := Balance(d)
	require.True(t, b.Equity.GreaterThan(b.Available))
	require.Equal(t, b.Equity.String(), b.Available.Add(b.Locked).String(),
		"equity is the free balance plus what our own orders hold")
}

// Отсутствующие или пустые поля не должны давать мусор: пустая строка обязана
// стать нулём, а не сломать разбор — иначе один пропуск в ответе биржи
// обнуляет или взрывает размер.
func TestMissingBalanceFieldsBecomeZero(t *testing.T) {
	var d dto.Balance
	require.NoError(t, json.Unmarshal([]byte(`{"equity":"","locked":"","cross_available":""}`), &d))

	b := Balance(d)
	require.True(t, b.Equity.IsZero())
	require.True(t, b.Available.IsZero())
	require.True(t, b.Locked.IsZero())
}

// Канал balance отдаёт кошелёк в другой форме, без сводных полей.
// Приближение equity берётся из total, и это должно быть видно в тесте,
// а не только в комментарии.
func TestWalletBalanceMapsTheChannelForm(t *testing.T) {
	var d dto.WalletBalance
	require.NoError(t, json.Unmarshal([]byte(`{
		"account_type":"perp","token":"DUSD",
		"free":"10275.90","locked":"815.97","occupied":"815.97","total":"11091.87"
	}`), &d))

	b := WalletBalance(d)
	require.Equal(t, "11091.87", b.Equity.String())
	require.Equal(t, "10275.9", b.Available.String())
	require.Equal(t, "815.97", b.Locked.String())
}
