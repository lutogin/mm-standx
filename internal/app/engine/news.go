package engine

import (
	"time"

	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/uptime"
	"github.com/nd/mm-standx/internal/ports"
)

// NewsGuard — как актор относится к запланированным публикациям.
type NewsGuard struct {
	// Enabled — включён ли блэкаут вовсе.
	Enabled bool
	// MaxAge — возраст, после которого расписанию перестают доверять.
	MaxAge time.Duration
	// FailOpen решает, что означает недоступное или устаревшее расписание:
	// котировать дальше по уже известному либо не котировать вовсе.
	FailOpen bool
	// DefensiveInsetBps — насколько внутрь от края полосы разрешено стоять
	// в защитной позе.
	DefensiveInsetBps money.Bps
	// DefensiveSizeMultiple — во сколько раз уменьшается размер в защитной позе.
	DefensiveSizeMultiple float64
}

// newsVerdict — что календарь требует от актора прямо сейчас.
type newsVerdict struct {
	// blocked непусто, когда котировать нельзя; cause объясняет это метрикам.
	blocked string
	cause   ports.WithdrawCause
	// defensive требует отойти к краю полосы и уменьшить размер.
	defensive bool
	// posture — поза для сводки. Хранится отдельно от полей выше, потому что
	// «жёсткая по календарю» и «жёсткая, потому что календарь не читается» —
	// разные вещи, и в логе они обязаны различаться.
	posture calendar.Posture
}

// assessNews решает, что делать с котировками ввиду ближайших публикаций.
//
// Три разных вопроса, намеренно разведённых:
//
//  1. Можно ли доверять расписанию. Недоступный издатель — не повод стоять
//     на публикации, но и не повод останавливаться на сутки; решает FailOpen.
//  2. Идёт ли публикация прямо сейчас. Это единственный отрезок, где никакая
//     дистанция внутри полосы не спасает, и ответ здесь один — отсутствие.
//  3. Переживёт ли час запланированное затемнение. Учёт аптайма считает
//     замеры и о будущем простое знать не может, поэтому списать его
//     заранее — единственный способ вовремя заметить, что час уже потрачен.
func (e *Engine) assessNews(now time.Time, hour uptime.Snapshot) newsVerdict {
	if !e.newsGuard.Enabled {
		return newsVerdict{}
	}

	if reason, ok := e.staleCalendar(now); ok {
		e.notePosture(calendar.Hard, reason)
		return newsVerdict{blocked: reason, cause: ports.CauseNewsUnavailable, posture: calendar.Hard}
	}

	verdict := e.news.Assess(now)
	e.notePosture(verdict.Posture, verdict.Reason)

	switch verdict.Posture {
	case calendar.Hard:
		return newsVerdict{
			blocked: "scheduled release blackout: " + verdict.Reason,
			cause:   ports.CauseNewsBlackout,
			posture: calendar.Hard,
		}
	case calendar.Defensive:
		if reason, spent := e.hourSpentOnNews(now, hour); spent {
			return newsVerdict{blocked: reason, cause: ports.CauseNewsHourSpent, posture: calendar.Hard}
		}
		return newsVerdict{defensive: true, posture: calendar.Defensive}
	default:
		return newsVerdict{}
	}
}

// staleCalendar сообщает, что расписанию нельзя доверять.
//
// Возраст здесь не про содержание: недельный файл покрывает неделю вперёд
// и от суточного возраста хуже не становится. Он про то, что расписание могли
// изменить — публикацию переносят, — а мы об этом не узнали.
func (e *Engine) staleCalendar(now time.Time) (string, bool) {
	if e.newsGuard.FailOpen {
		return "", false
	}
	if e.newsFetched.IsZero() {
		return "the economic calendar has never been read", true
	}
	if age := now.Sub(e.newsFetched); age > e.newsGuard.MaxAge {
		return "the economic calendar is " + age.Truncate(time.Minute).String() + " old", true
	}
	return "", false
}

// hourSpentOnNews сообщает, что запланированное затемнение уже отняло у часа
// возможность окупиться.
//
// Считается заранее и только в режиме накопления. В Banked цель уже взята
// и простой ничего не отнимает; в Lost актор и так снимает котировки, и второе
// объяснение тому же исходу лишь запутало бы разбор.
//
// Смысл проверки в том, чего сам учёт аптайма сделать не может. Он считает
// замеры, то есть знает только прошлое; публикация через одиннадцать минут —
// это простой, который наверняка будет, и без предварительного списания актор
// котировал бы ещё четверть часа под полным риском исполнения ради часа,
// который к тому моменту уже не оплатится.
func (e *Engine) hourSpentOnNews(now time.Time, hour uptime.Snapshot) (string, bool) {
	if hour.Mode != uptime.Earning {
		return "", false
	}

	endOfHour := now.UTC().Truncate(time.Hour).Add(time.Hour)
	planned := e.news.Blackout(now, endOfHour)
	if planned <= hour.DowntimeBudget {
		return "", false
	}

	return "a scheduled blackout of " + planned.Truncate(time.Second).String() +
		" exceeds the " + hour.DowntimeBudget.Truncate(time.Second).String() +
		" of downtime this hour can still absorb", true
}

// newsSummary — короткая строка о календаре для сводки.
//
// Несёт и позу, и время ближайшей публикации: вопрос «почему бот сейчас стоит
// далеко» и вопрос «когда он уйдёт из стакана» задаются вместе, и отвечать
// на них двумя строками в разных местах лога значит заставлять сводить их
// руками.
func (e *Engine) newsSummary(now time.Time) string {
	if !e.newsGuard.Enabled {
		return "off"
	}
	if e.newsFetched.IsZero() {
		return "no calendar"
	}

	out := e.news.Assess(now).Posture.String()
	if next, ok := e.news.Next(now); ok {
		out += ", next in " + next.At.Sub(now).Truncate(time.Minute).String()
	} else {
		out += ", nothing scheduled"
	}
	return out
}

// notePosture печатает смену позы один раз.
//
// Календарь тих часами, поэтому событие здесь — именно переход. Уровни разные
// намеренно: уход из стакана перед публикацией обязан быть заметен в логе
// без поиска, а возвращение и отход — нет.
func (e *Engine) notePosture(next calendar.Posture, reason string) {
	if next == e.newsPosture {
		return
	}
	previous := e.newsPosture
	e.newsPosture = next

	switch next {
	case calendar.Hard:
		e.deps.Log.Warn("leaving the book for a scheduled release",
			"symbol", e.symbol, "reason", reason)
	case calendar.Defensive:
		e.deps.Log.Info("standing back for an upcoming release",
			"symbol", e.symbol, "reason", reason,
			"inset", e.newsGuard.DefensiveInsetBps,
			"size_multiple", e.newsGuard.DefensiveSizeMultiple)
	default:
		e.deps.Log.Info("the release window has passed, quoting normally",
			"symbol", e.symbol, "previous", previous.String())
	}
}
