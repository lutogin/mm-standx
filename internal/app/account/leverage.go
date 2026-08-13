package account

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/ports"
)

// ApplyLeverage приводит плечо счёта к настроенному и убеждается, что оно
// применилось.
//
// Плечо счёта — единственное, что определяет блокировку маржи под висящим
// ордером: поле leverage в самом ордере биржа принимает и игнорирует (проба
// 12 августа, PLAN.md §12.5). Вызова этой настройки в проекте не было вовсе,
// поэтому счёт всё время работал на значении биржи по умолчанию — эффективное
// плечо 9.49 вместо доступных десятков, то есть втрое меньший размер на те же
// деньги.
//
// Результат перечитывается, а не выводится из ответа. Успешный ответ означает
// приём запроса; настройка, которая не применилась, не проявилась бы ничем —
// бот продолжил бы считать размер по конфигурации, а биржа блокировала бы
// маржу по-своему, и расхождение обнаружилось бы только через недобранное
// начисление.
//
// Ошибка не останавливает запуск. Прежнее плечо — это меньший размер, а не
// опасность: рисковать оно не заставляет, и падать из-за него значило бы менять
// работающего бота на неработающего.
func ApplyLeverage(
	ctx context.Context,
	cfg ports.PositionConfigurer,
	symbols map[market.Symbol]int,
	log *slog.Logger,
) {
	for symbol, want := range symbols {
		before, err := cfg.PositionConfig(ctx, symbol)
		if err != nil {
			log.Error("cannot read the account leverage, leaving it alone",
				"symbol", symbol, "error", err)
			continue
		}
		if before.Leverage == want {
			log.Info("account leverage already matches the configuration",
				"symbol", symbol, "leverage", want, "margin_mode", before.MarginMode)
			continue
		}

		if err := cfg.SetLeverage(ctx, symbol, want); err != nil {
			log.Error("failed to set the account leverage, size stays limited by the old one",
				"symbol", symbol, "from", before.Leverage, "to", want, "error", err)
			continue
		}

		after, err := cfg.PositionConfig(ctx, symbol)
		if err != nil {
			log.Error("leverage was accepted but could not be verified",
				"symbol", symbol, "requested", want, "error", err)
			continue
		}
		if after.Leverage != want {
			log.Error("the exchange accepted the leverage and did not apply it",
				"symbol", symbol, "requested", want, "in_force", after.Leverage,
				"effect", "sizing will assume more capacity than the account actually has")
			continue
		}
		log.Warn("account leverage changed",
			"symbol", symbol, "from", before.Leverage, "to", after.Leverage,
			"effect", fmt.Sprintf("a dollar of margin now carries about %d dollars of notional", want))
	}
}
