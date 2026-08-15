package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// utilizationPct — предел риска, до 15 августа жил константой в коде актора
// (85%, до этого 70% и 95% — менялось без единого коммита, отслеживаемого
// правкой файла). Единственный среди соседних пределов в этом файле, который
// нельзя было ни увидеть в работающем процессе, ни поправить без пересборки
// образа. Тесты здесь закрывают ровно то, ради чего он переехал: значение
// приходит из файла, а не устаревшей строкой в бинарнике.

// withUtilization подставляет ключ внутрь существующего блока risk:
// minimalConfig, а не приписывает его в конец файла — YAML не прощает ключ
// верхнего уровня там, где ожидается продолжение предыдущего блока.
func withUtilization(value string) string {
	return strings.Replace(minimalConfig,
		"risk:\n  maxFillsPerHour: 6",
		"risk:\n  maxFillsPerHour: 6\n  utilizationPct: "+value, 1)
}

func TestUtilizationPctIsReadFromTheFile(t *testing.T) {
	cfg, err := Load(writeConfig(t, withUtilization("62.5")))
	require.NoError(t, err)
	require.Equal(t, 62.5, cfg.Risk.UtilizationPct)
}

func TestUtilizationPctDefaultsToASafeValue(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig))
	require.NoError(t, err)
	// Значение по умолчанию обязано быть задано и не должно превышать боевое
	// (85 в config.yaml): умолчания не имеют права быть агрессивнее явно
	// прописанной конфигурации.
	require.Greater(t, cfg.Risk.UtilizationPct, 0.0)
	require.LessOrEqual(t, cfg.Risk.UtilizationPct, 85.0)
}

func TestUtilizationPctMustBePositive(t *testing.T) {
	_, err := Load(writeConfig(t, withUtilization("0")))
	require.ErrorContains(t, err, "risk.utilizationPct")
}

func TestUtilizationPctMustNotExceedTheWholeAccount(t *testing.T) {
	// 100% означает отсутствие резерва под закрывающий ордер после залива —
	// допускать значения выше физически бессмысленно.
	_, err := Load(writeConfig(t, withUtilization("100.1")))
	require.ErrorContains(t, err, "risk.utilizationPct")
}

func TestUtilizationPctAtExactlyTheAccountIsAllowed(t *testing.T) {
	cfg, err := Load(writeConfig(t, withUtilization("100")))
	require.NoError(t, err)
	require.Equal(t, 100.0, cfg.Risk.UtilizationPct)
}

func TestUtilizationPctRejectsNegativeValues(t *testing.T) {
	_, err := Load(writeConfig(t, withUtilization("-5")))
	require.ErrorContains(t, err, "risk.utilizationPct")
}
