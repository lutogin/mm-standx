# StandX Perps API — рабочая выжимка

Составлено из [docs.standx.com](https://docs.standx.com/standx-api/standx-api) по состоянию
на 2026-08. Здесь только то, что нужно боту. Оригинал — источник истины при расхождениях.

## Хосты

| Назначение | URL |
| --- | --- |
| Auth | `https://api.standx.com` |
| REST | `https://perps.standx.com` |
| Market/User WS | `wss://perps.standx.com/ws-stream/v1` |
| Order WS API | `wss://perps.standx.com/ws-api/v1` |
| Регион и время сервера | `https://geo.standx.com/v1/region` |

## Авторизация

Двухключевая схема: кошелёк доказывает личность один раз, эфемерный ed25519-ключ
подписывает каждый торговый запрос.

### Получение JWT

1. Сгенерировать ed25519-пару. `requestId = base58(publicKey)`.
2. `POST /v1/offchain/prepare-signin?chain=bsc` с телом `{address, requestId}`
   → `{success: true, signedData: "<JWT>"}`.
3. Распарсить payload `signedData` (base64url средней секции), взять поле `message`.
   Проверить подпись публичным ключом из `GET /v1/offchain/certs`.
4. Подписать `message` приватным ключом кошелька (EIP-191 `personal_sign` для BSC).
5. `POST /v1/offchain/login?chain=bsc` с телом `{signature, signedData, expiresSeconds}`
   → `{token, address, chain}`.

`chain` — `bsc` или `solana`. Токен по умолчанию живёт 7 дней (`604800`).

### Подпись тела запроса

Торговые эндпойнты требуют дополнительно к `Authorization: Bearer <jwt>`:

```
x-request-sign-version: v1
x-request-id:           <uuid v4, новый на каждый запрос>
x-request-timestamp:    <unix ms>
x-request-signature:    base64(ed25519.Sign("v1,{x-request-id},{x-request-timestamp},{body}"))
```

Подписывается **точно та байтовая строка тела**, которая уходит в запросе — сериализовать
JSON нужно один раз и использовать один и тот же буфер для подписи и для отправки.

Осторожно: `requestId` в `prepare-signin` (base58 публичного ключа) и заголовок
`x-request-id` (UUID на запрос) — разные значения, несмотря на похожие имена.

### Session ID

Для `new_order` и `cancel_order` заголовок `x-session-id` связывает REST-запрос
с WS-потоком ответов. Должен совпадать с `session_id` в ws-клиенте, иначе результат
матчинга получить неоткуда.

## Торговые эндпойнты

Все требуют JWT + подпись тела.

### `POST /api/new_order`

| Поле | Тип | Примечание |
| --- | --- | --- |
| `symbol` | string | |
| `side` | enum | `buy` \| `sell` |
| `order_type` | enum | `limit` \| `market` |
| `qty` | decimal-**строка** | |
| `time_in_force` | enum | `gtc` \| `ioc` \| `alo` |
| `reduce_only` | bool | |
| `price` | decimal-строка | обязательно для `limit` |
| `cl_ord_id` | string | наш ULID; если не задан — генерирует биржа |
| `margin_mode` | enum | `cross` \| `isolated`, должен совпадать с позицией |
| `leverage` | int | должен совпадать с позицией |
| `tp_price` / `sl_price` | decimal-строка | создают TP/SL после исполнения |

Ответ `{code: 0, message: "success", request_id}` означает **принято к обработке**,
а не исполнено. Реальный результат (включая отклонение ALO) приходит в Order Response Stream.

### `POST /api/cancel_order`
Тело `{order_id}` или `{cl_ord_id}`. Тоже асинхронно.

### `POST /api/cancel_orders`
Тело `{order_id_list: []}` или `{cl_ord_id_list: []}`. **Один запрос на пачку** —
основной инструмент аварийного съёма стороны.

> **Ответ приходит массивом конвертов**, по одному на ордер, а не одиночным
> объектом как в документации. Проверено на боевом счёте. Разбор по документации
> проваливается на каждом вызове, и клиент молча уходит в запасной канал.

### `POST /api/change_leverage`, `POST /api/change_margin_mode`, `POST /api/transfer_margin`
`{symbol, leverage}` / `{symbol, margin_mode}` / `{symbol, amount_in}`.

**Amend/modify отсутствует.** Перекотировка — это всегда cancel + new.

## Чтение состояния (JWT, без подписи тела)

| Эндпойнт | Назначение |
| --- | --- |
| `GET /api/query_order` | по `order_id` или `cl_ord_id` |
| `GET /api/query_orders` | история, фильтры `symbol/status/order_type/start/end`, `limit` ≤ 500 |
| `GET /api/query_open_orders` | **основа сверки**, `limit` ≤ 1200 |
| `GET /api/query_trades` | исполнения с `fee_qty`, `pnl`, `value` |
| `GET /api/query_positions` | `qty`, `entry_price`, `liq_price`, `upnl`, `margin_mode` |
| `GET /api/query_balance` | сводка баланса, см. ниже |
| `GET /api/query_position_config` | `{symbol, leverage, margin_mode}` |
| `GET /api/query_funding_history` | выплаты funding |

### Поля `query_balance`

| Поле | Смысл |
| --- | --- |
| `cross_balance` | свободный баланс cross-кошелька |
| `cross_margin` | маржа под **исполненными** позициями |
| `locked` | блокировка под **висящими ордерами** (маржа + комиссия, с коэффициентом запаса) |
| `cross_available` | `cross_balance − cross_margin − locked + cross_upnl`, может быть отрицательным |
| `equity` | `balance + upnl` |

`locked` — тот самый параметр, который определяет, сколько капитала реально съедают
висящие котировки. Открытый вопрос PLAN.md §12.5.

## Публичные эндпойнты

| Эндпойнт | Назначение |
| --- | --- |
| `GET /api/query_market_overview` | **список всех символов** + mark price, funding, OI, объём |
| `GET /api/query_symbol_info` | спецификация символа, см. ниже |
| `GET /api/query_symbol_market` | mark/index/last/mid, спред, funding, next_funding_time |
| `GET /api/query_symbol_price` | облегчённая версия предыдущего |
| `GET /api/query_depth_book` | стакан; **порядок уровней не гарантирован, сортировать локально** |
| `GET /api/query_recent_trades` | последние сделки |
| `GET /api/kline/history` | свечи, `resolution`: `1T,3S,1,5,15,60,1D,1W,1M` |
| `GET /api/health` | `OK` |

### `query_symbol_info` — что берём

```json
{
  "symbol": "BTC-USD",
  "price_tick_decimals": 2,     // знаков после запятой в цене
  "qty_tick_decimals": 4,       // знаков после запятой в количестве
  "min_order_qty": "0.0001",
  "max_order_qty": "100",
  "max_open_orders": "100",     // потолок на лесенку
  "max_position_size": "1000",
  "max_leverage": "20",
  "def_leverage": "10",
  "maker_fee": "0.0001",        // 1 bp
  "taker_fee": "0.0004",        // 4 bps — цена каждого закрытия по маркету
  "price_cap_ratio": "0.3",
  "price_floor_ratio": "0.3",
  "enabled": true
}
```

Список символов в официальном справочнике устарел (перечислены 4 из 11 реально
торгуемых). **Символы читаем рантаймом** из `query_market_overview`, спецификации —
из `query_symbol_info`. Ничего не хардкодим.

## WebSocket

### Общее для обоих потоков
- Соединение принудительно рвётся через **24 часа** → плановая ротация раньше срока.
- Сервер шлёт ping каждые 10 с; без pong в течение 5 минут — разрыв с кодом 408.
- Лимиты: 10 соединений на IP, 30 новых соединений в минуту.

> **Сервер не отвечает на клиентские ping.** Направление ping/pong здесь
> одностороннее: сервер спрашивает, клиент отвечает. Обратное не работает —
> проверено на живом соединении, где `Conn.Ping` стабильно истекал по таймауту
> при активном читателе и потоке данных в 3.8 сообщения в секунду. Наивная
> проверка живости пингом рвала исправное соединение ровно через
> `pingInterval + pingTimeout`.
>
> Живость определяется по молчанию: каждое чтение ограничено дедлайном
> `wsIdleTimeout`. Каналы, по которым данные не идут сами (торговый канал,
> пользовательские подписки), обязаны поддерживать поток ответов прикладным
> запросом — повторным `auth:login` либо подпиской на `price` любого инструмента.

### Market Stream — `wss://perps.standx.com/ws-stream/v1`

Подписка: `{"subscribe": {"channel": "<name>", "symbol": "<symbol>"}}`.
Авторизация: `{"auth": {"token": "<jwt>", "streams": [{"channel": "order"}]}}`.

> Успешный ответ авторизации приходит с `code: 0`, а не `200` как в документации.
> Проверять следует наличие кода ошибки, а не равенство двумстам.

| Канал | Символ | Содержимое |
| --- | --- | --- |
| `price` | да | **`mark_price`** — основной вход стратегии, плюс index/last/mid/spread |
| `depth_book` | да | `asks`/`bids` как `[["цена","объём"], ...]`, порядок не гарантирован |
| `public_trade` | да | публичные сделки |
| `order` | нет | обновления наших ордеров (нужна авторизация) |
| `position` | нет | обновления позиций |
| `balance` | нет | обновления баланса |
| `trade` | нет | наши исполнения |

Каждое сообщение содержит `seq` — использовать для детекта пропусков.

### Order Response Stream — `wss://perps.standx.com/ws-api/v1`

Горячий путь размещения и отмены, минуя REST.

```json
{
  "session_id": "<uuid, постоянный на сессию>",
  "request_id": "<uuid, уникальный на запрос>",
  "method": "auth:login | order:new | order:cancel",
  "header": {
    "x-request-id": "", "x-request-timestamp": "", "x-request-signature": ""
  },
  "params": "<JSON-строка, тот же payload что у REST>"
}
```

`params` — именно **строка** с JSON внутри, не вложенный объект. `header` обязателен
для `order:new` и `order:cancel`.

Ответы:
```json
{"code": 0, "status": "accepted", "request_id": "..."}   // прошло гейтвей
{"code": 0, "message": "success", "request_id": "..."}   // принято
{"code": 400, "message": "alo order rejected", "request_id": "..."}
```

На один запрос приходит **два сообщения**: сначала `accepted` от шлюза, затем
итог матчинга. Таблица ожидания должна переживать первый ответ, иначе отклонение
ALO окажется невозможно связать с клиентским идентификатором ордера.

**Пакетной отмены в этом канале нет** — только `order:new` и `order:cancel`.
Снятие пачки идёт либо конвейером одиночных команд (N кредитов, один оборот
сети), либо через REST `cancel_orders` (один кредит, один оборот). Выбор
и правила отката — в PLAN.md §2.2.

Открытый вопрос: расходуют ли сообщения торгового канала тот же бюджет кредитов,
что и REST. Документация умалчивает, поэтому клиент считает, что да — переоценка
расхода стоит доли миллисекунды, недооценка приводит к отказу `429`.

## Лимиты запросов

| Уровень | Лимит |
| --- | --- |
| Аккаунт | 45 кредитов на запрос, пополнение 1000/с, бакет 900 (20 запросов) → **~22 rps** |
| IP | 50 rps |
| WS | 10 соединений на IP, 30 новых в минуту |

Превышение → HTTP `429`. Наш шейпер настроен на 15 rps с резервом кредитов
под команды безопасности (PLAN.md §4.4).

## Перечисления

- `side`: `buy`, `sell`
- `order_type`: `limit`, `market`
- `time_in_force`: `gtc`, `ioc`, **`alo`** (post-only — отклоняется при пересечении)
- `status`: `open`, `canceled`, `filled`, `rejected`, `untriggered`
- `margin_mode`: `cross`, `isolated`
- Коды ошибок: `400`, `401`, `403`, `404`, `429`, `500`

## Грабли

1. **Всё асинхронно.** Успешный HTTP-ответ ≠ ордер в стакане. Единственный источник
   истины о состоянии ордера — канал `order`, а не ответ на запрос.
2. **`decimal` передаётся строками**, целые — числами. Отправка цены как JSON-числа —
   ошибка.
3. **Порядок уровней стакана не гарантирован** ни в REST, ни в WS. Сортировать локально.
4. **Нет cancel-on-disconnect.** Если процесс умрёт, ордера останутся в стакане
   и будут исполняться. Dead-man's switch обязателен.
5. **Нет amend.** Лесенка ордеров — единственный способ не терять присутствие
   в стакане при переезде.
6. `x-session-id` без совпадающего ws-клиента = результаты матчинга не придут.
7. Подписывать нужно ту же самую байтовую строку тела, что уходит в сеть.
