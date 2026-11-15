package gormquery

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gen/field"
)

func TestDateTimeRangeExpr(t *testing.T) {
	db := newTestDB(t)
	column := field.NewTime("events", "created_at")

	t.Run("both ends produce between", func(t *testing.T) {
		rangeValue := &DateTimeRange{
			StartTime: "2026-01-02 03:04:05",
			EndTime:   "2026-02-03 04:05:06",
		}
		expression := rangeValue.Expr(column)
		require.NotNil(t, expression)

		sql, vars := render(t, db, expression)
		assert.Equal(t, "`events`.`created_at` BETWEEN ? AND ?", sql)
		require.Len(t, vars, 2)
		assert.Equal(t,
			time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			vars[0],
		)
		assert.Equal(t,
			time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
			vars[1],
		)
	})

	t.Run("missing end yields nil", func(t *testing.T) {
		assert.Nil(t, (&DateTimeRange{StartTime: "2026-01-02 03:04:05"}).Expr(column))
	})

	t.Run("missing start yields nil", func(t *testing.T) {
		assert.Nil(t, (&DateTimeRange{EndTime: "2026-02-03 04:05:06"}).Expr(column))
	})

	t.Run("invalid value yields nil", func(t *testing.T) {
		rangeValue := &DateTimeRange{StartTime: "not a date", EndTime: "2026-02-03 04:05:06"}
		assert.Nil(t, rangeValue.Expr(column))
	})

	t.Run("nil receiver yields nil", func(t *testing.T) {
		var rangeValue *DateTimeRange
		assert.Nil(t, rangeValue.Expr(column))
	})
}

func TestDateTimeRangeToExpr(t *testing.T) {
	db := newTestDB(t)
	column := field.NewTime("events", "created_at")

	t.Run("clears fields after conversion", func(t *testing.T) {
		rangeValue := &DateTimeRange{
			StartTime: "2026-01-02 03:04:05",
			EndTime:   "2026-02-03 04:05:06",
		}
		expression := rangeValue.ToExpr(column)
		require.NotNil(t, expression)
		sql, _ := render(t, db, expression)
		assert.Equal(t, "`events`.`created_at` BETWEEN ? AND ?", sql)
		assert.Empty(t, rangeValue.StartTime)
		assert.Empty(t, rangeValue.EndTime)
	})

	t.Run("keeps fields when no expression is produced", func(t *testing.T) {
		rangeValue := &DateTimeRange{StartTime: "2026-01-02 03:04:05"}
		assert.Nil(t, rangeValue.ToExpr(column))
		assert.Equal(t, "2026-01-02 03:04:05", rangeValue.StartTime)
		assert.Empty(t, rangeValue.EndTime)
	})

	t.Run("pure resolver leaves fields in place", func(t *testing.T) {
		rangeValue := &DateTimeRange{
			StartTime: "2026-01-02 03:04:05",
			EndTime:   "2026-02-03 04:05:06",
		}
		require.NotNil(t, rangeValue.Expr(column))
		assert.Equal(t, "2026-01-02 03:04:05", rangeValue.StartTime)
		assert.Equal(t, "2026-02-03 04:05:06", rangeValue.EndTime)
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var rangeValue *DateTimeRange
		assert.Nil(t, rangeValue.ToExpr(column))
	})
}
