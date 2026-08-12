package calendar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	// Полдень как момент публикации: CPI и payrolls выходят в 12:30 UTC.
	release = time.Date(2026, 8, 12, 12, 30, 0, 0, time.UTC)

	policy = Policy{
		DefensiveBefore: 15 * time.Minute,
		DefensiveAfter:  15 * time.Minute,
		HardBefore:      time.Minute,
		HardAfter:       time.Minute,
	}
)

func cpi(at time.Time) Event {
	return Event{At: at, Title: "CPI m/m", Currency: "USD", Impact: ImpactHigh}
}

func TestPostureWidensThenClearsAroundARelease(t *testing.T) {
	c := New(policy, []Event{cpi(release)})

	cases := []struct {
		offset time.Duration
		want   Posture
	}{
		{-20 * time.Minute, Normal},
		{-15 * time.Minute, Defensive},
		{-2 * time.Minute, Defensive},
		{-time.Minute, Hard},
		{0, Hard},
		{time.Minute, Hard},
		{90 * time.Second, Defensive},
		{15 * time.Minute, Defensive},
		{16 * time.Minute, Normal},
	}
	for _, tc := range cases {
		got := c.Assess(release.Add(tc.offset))
		require.Equal(t, tc.want, got.Posture, "at %s", tc.offset)
	}
}

func TestVerdictNamesTheRelease(t *testing.T) {
	c := New(policy, []Event{cpi(release)})

	v := c.Assess(release)

	require.Equal(t, Hard, v.Posture)
	require.Contains(t, v.Reason, "CPI m/m")
	require.Equal(t, release.Add(time.Minute), v.Until)
}

// Календарь публикует Core CPI m/m, Core CPI y/y, CPI m/m и CPI y/y одним
// временем. Защитное окно одного не имеет права смягчить жёсткое окно другого.
func TestStrictestPostureWinsWhenWindowsOverlap(t *testing.T) {
	c := New(policy, []Event{
		cpi(release),
		{At: release.Add(10 * time.Minute), Title: "Retail Sales", Currency: "USD", Impact: ImpactHigh},
	})

	// Внутри жёсткого окна первого события и защитного окна второго.
	v := c.Assess(release)

	require.Equal(t, Hard, v.Posture)
}

// Из двух одинаковых поз выигрывает та, что кончается позже: иначе после
// первого события бот вернулся бы в стакан прямо посреди защитного окна второго.
func TestOverlappingEqualPosturesKeepTheLaterEnd(t *testing.T) {
	c := New(policy, []Event{
		cpi(release),
		cpi(release.Add(20 * time.Minute)),
	})

	v := c.Assess(release.Add(10 * time.Minute))

	require.Equal(t, Defensive, v.Posture)
	require.Equal(t, release.Add(35*time.Minute), v.Until)
}

func TestEmptyCalendarDemandsNothing(t *testing.T) {
	require.Equal(t, Normal, Calendar{}.Assess(release).Posture)
	require.Equal(t, Normal, New(policy, nil).Assess(release).Posture)
}

func TestBlackoutMeasuresTheHardWindowInsideTheInterval(t *testing.T) {
	c := New(policy, []Event{cpi(release)})
	hour := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	require.Equal(t, 2*time.Minute, c.Blackout(hour, hour.Add(time.Hour)),
		"one minute either side of the release")
	require.Equal(t, time.Duration(0), c.Blackout(hour, hour.Add(20*time.Minute)),
		"a release later in the hour is not yet inside the interval")
	require.Equal(t, time.Minute, c.Blackout(release, hour.Add(time.Hour)),
		"only the half still ahead of us counts")
}

// Четыре строки CPI в один момент — это одно затемнение, а не четыре.
// Без слияния бюджет часа списывался бы кратно числу строк в календаре.
func TestBlackoutMergesSimultaneousReleases(t *testing.T) {
	c := New(policy, []Event{
		{At: release, Title: "Core CPI m/m", Currency: "USD", Impact: ImpactHigh},
		{At: release, Title: "Core CPI y/y", Currency: "USD", Impact: ImpactHigh},
		{At: release, Title: "CPI m/m", Currency: "USD", Impact: ImpactHigh},
		{At: release, Title: "CPI y/y", Currency: "USD", Impact: ImpactHigh},
	})
	hour := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	require.Equal(t, 2*time.Minute, c.Blackout(hour, hour.Add(time.Hour)))
}

// Близкие, но не совпадающие публикации сливаются в одно окно, когда их
// затемнения соприкасаются.
func TestBlackoutMergesAdjacentWindows(t *testing.T) {
	c := New(policy, []Event{cpi(release), cpi(release.Add(90 * time.Second))})
	hour := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	require.Equal(t, 3*time.Minute+30*time.Second, c.Blackout(hour, hour.Add(time.Hour)),
		"12:29:00 to 12:32:30 is one continuous blackout")
}

func TestBlackoutSumsDistinctWindows(t *testing.T) {
	c := New(policy, []Event{cpi(release), cpi(release.Add(20 * time.Minute))})
	hour := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	require.Equal(t, 4*time.Minute, c.Blackout(hour, hour.Add(time.Hour)))
}

// Окно, переходящее границу часа, засчитывается часу только своей половиной:
// бюджет простоя считается внутри календарного часа, и списывать ему чужую
// минуту значило бы потерять час, который ещё оплачивался.
func TestBlackoutCountsOnlyTheOverlappingPart(t *testing.T) {
	edge := time.Date(2026, 8, 12, 13, 0, 0, 0, time.UTC)
	c := New(policy, []Event{cpi(edge)})
	hour := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	require.Equal(t, time.Minute, c.Blackout(hour, edge))
	require.Equal(t, time.Minute, c.Blackout(edge, edge.Add(time.Hour)))
}

func TestBlackoutOfAnEmptyIntervalIsZero(t *testing.T) {
	c := New(policy, []Event{cpi(release)})

	require.Equal(t, time.Duration(0), c.Blackout(release, release))
	require.Equal(t, time.Duration(0), c.Blackout(release.Add(time.Hour), release))
}

// Пустой список фильтра не пропускает ничего. Обратное соглашение —
// «пусто значит всё» — обнуляло бы стакан на каждой мелкой цифре недели.
func TestEmptyFilterMatchesNothing(t *testing.T) {
	require.False(t, Filter{}.Match(cpi(release)))
	require.False(t, Filter{Currencies: []string{"USD"}}.Match(cpi(release)))
	require.False(t, Filter{Impacts: []Impact{ImpactHigh}}.Match(cpi(release)))
}

func TestFilterSelectsByCurrencyAndImpact(t *testing.T) {
	f := Filter{Currencies: []string{"USD"}, Impacts: []Impact{ImpactHigh}}
	events := []Event{
		{At: release.Add(time.Hour), Title: "late", Currency: "USD", Impact: ImpactHigh},
		{At: release, Title: "on time", Currency: "usd", Impact: ImpactHigh},
		{At: release, Title: "wrong currency", Currency: "EUR", Impact: ImpactHigh},
		{At: release, Title: "wrong impact", Currency: "USD", Impact: ImpactLow},
	}

	got := f.Select(events)

	require.Len(t, got, 2)
	require.Equal(t, "on time", got[0].Title, "the result has to come back in chronological order")
	require.Equal(t, "late", got[1].Title)
}

func TestParseImpactNormalisesTheCalendarSpelling(t *testing.T) {
	require.Equal(t, ImpactHigh, ParseImpact("High"))
	require.Equal(t, ImpactHoliday, ParseImpact(" Holiday "))
}

// Жёсткое окно шире защитного почти наверняка означает опечатку: настройка
// говорит «отойти и продолжать», а поведение вышло бы «уйти совсем».
func TestPolicyRejectsAHardWindowWiderThanTheDefensiveOne(t *testing.T) {
	require.NoError(t, policy.Validate())

	require.ErrorContains(t,
		Policy{DefensiveBefore: time.Minute, HardBefore: 2 * time.Minute}.Validate(),
		"hardBefore")
	require.ErrorContains(t,
		Policy{DefensiveAfter: time.Minute, HardAfter: 2 * time.Minute}.Validate(),
		"hardAfter")
	require.ErrorContains(t,
		Policy{DefensiveBefore: -time.Minute}.Validate(),
		"must not be negative")
}

func TestNextFindsTheUpcomingRelease(t *testing.T) {
	c := New(policy, []Event{cpi(release), cpi(release.Add(time.Hour))})

	next, ok := c.Next(release.Add(time.Minute))
	require.True(t, ok)
	require.Equal(t, release.Add(time.Hour), next.At)

	_, ok = c.Next(release.Add(2 * time.Hour))
	require.False(t, ok)
}
