package gormkit

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
			time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local),
			vars[0],
		)
		assert.Equal(t,
			time.Date(2026, 2, 3, 4, 5, 6, 0, time.Local),
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

func TestDateTimeRangeExprLocalZone(t *testing.T) {
	db := newTestDB(t)
	column := field.NewTime("events", "created_at")

	previousLocal := time.Local
	time.Local = time.FixedZone("UTC+8", 8*3600)
	defer func() { time.Local = previousLocal }()

	rangeValue := &DateTimeRange{
		StartTime: "2026-01-02 19:38:00",
		EndTime:   "2026-01-02 20:38:00",
	}
	_, vars := render(t, db, rangeValue.Expr(column))
	require.Len(t, vars, 2)

	start, ok := vars[0].(time.Time)
	require.True(t, ok, "start bound is not a time.Time")
	assert.Equal(t, "2026-01-02 19:38:00", start.Format(time.DateTime))
	assert.True(t, start.Equal(time.Date(2026, 1, 2, 19, 38, 0, 0, time.Local)),
		"start bound should be 19:38 in the process local zone")
	assert.Equal(t, "2026-01-02 11:38:00", start.UTC().Format(time.DateTime))

	end, ok := vars[1].(time.Time)
	require.True(t, ok, "end bound is not a time.Time")
	assert.Equal(t, "2026-01-02 20:38:00", end.Format(time.DateTime))
	assert.Equal(t, "2026-01-02 12:38:00", end.UTC().Format(time.DateTime))
}

func TestDateTimeRangeExprIn(t *testing.T) {
	db := newTestDB(t)
	column := field.NewTime("events", "created_at")
	shanghai := time.FixedZone("UTC+8", 8*3600)

	rangeValue := &DateTimeRange{
		StartTime: "2026-01-02 19:38:00",
		EndTime:   "2026-01-02 20:38:00",
	}

	t.Run("zones shift the bounds", func(t *testing.T) {
		_, utcVars := render(t, db, rangeValue.ExprIn(column, time.UTC))
		_, shanghaiVars := render(t, db, rangeValue.ExprIn(column, shanghai))
		require.Len(t, utcVars, 2)
		require.Len(t, shanghaiVars, 2)

		utcStart := utcVars[0].(time.Time)
		shanghaiStart := shanghaiVars[0].(time.Time)
		assert.Equal(t, "2026-01-02 19:38:00", utcStart.UTC().Format(time.DateTime))
		assert.Equal(t, "2026-01-02 11:38:00", shanghaiStart.UTC().Format(time.DateTime))
		assert.Equal(t, 8*time.Hour, utcStart.Sub(shanghaiStart))
	})

	t.Run("nil zone falls back to local", func(t *testing.T) {
		previousLocal := time.Local
		time.Local = time.FixedZone("UTC+8", 8*3600)
		defer func() { time.Local = previousLocal }()

		_, nilVars := render(t, db, rangeValue.ExprIn(column, nil))
		_, localVars := render(t, db, rangeValue.ExprIn(column, time.Local))
		require.Len(t, nilVars, 2)
		require.Len(t, localVars, 2)
		assert.Equal(t, localVars, nilVars)
	})

	t.Run("missing or invalid end yields nil", func(t *testing.T) {
		assert.Nil(t, (&DateTimeRange{StartTime: "2026-01-02 19:38:00"}).ExprIn(column, time.UTC))
		assert.Nil(t,
			(&DateTimeRange{StartTime: "not a date", EndTime: "2026-01-02 20:38:00"}).ExprIn(column, time.UTC))
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

func TestDateTimeRangeToExprIn(t *testing.T) {
	db := newTestDB(t)
	column := field.NewTime("events", "created_at")

	t.Run("clears fields after conversion in the given zone", func(t *testing.T) {
		rangeValue := &DateTimeRange{
			StartTime: "2026-01-02 19:38:00",
			EndTime:   "2026-01-02 20:38:00",
		}
		expression := rangeValue.ToExprIn(column, time.UTC)
		require.NotNil(t, expression)
		sql, vars := render(t, db, expression)
		assert.Equal(t, "`events`.`created_at` BETWEEN ? AND ?", sql)
		require.Len(t, vars, 2)
		assert.Equal(t, "2026-01-02 19:38:00", vars[0].(time.Time).UTC().Format(time.DateTime))
		assert.Empty(t, rangeValue.StartTime)
		assert.Empty(t, rangeValue.EndTime)
	})

	t.Run("keeps fields when no expression is produced", func(t *testing.T) {
		rangeValue := &DateTimeRange{StartTime: "2026-01-02 19:38:00"}
		assert.Nil(t, rangeValue.ToExprIn(column, time.UTC))
		assert.Equal(t, "2026-01-02 19:38:00", rangeValue.StartTime)
		assert.Empty(t, rangeValue.EndTime)
	})

	t.Run("nil zone falls back to local", func(t *testing.T) {
		previousLocal := time.Local
		time.Local = time.FixedZone("UTC+8", 8*3600)
		defer func() { time.Local = previousLocal }()

		rangeValue := &DateTimeRange{
			StartTime: "2026-01-02 19:38:00",
			EndTime:   "2026-01-02 20:38:00",
		}
		expression := rangeValue.ToExprIn(column, nil)
		require.NotNil(t, expression)
		_, vars := render(t, db, expression)
		require.Len(t, vars, 2)
		assert.Equal(t, "2026-01-02 11:38:00", vars[0].(time.Time).UTC().Format(time.DateTime))
		assert.Empty(t, rangeValue.StartTime)
		assert.Empty(t, rangeValue.EndTime)
	})
}
