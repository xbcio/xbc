package gormx

import (
	"time"

	"gorm.io/gen/field"
)

// DateTimeRange binds the inclusive start and end of a time range. The binding
// tag rejects values that do not use the "2006-01-02 15:04:05" layout.
type DateTimeRange struct {
	StartTime string `form:"start_time" binding:"omitempty,datetime=2006-01-02 15:04:05"`
	EndTime   string `form:"end_time" binding:"omitempty,datetime=2006-01-02 15:04:05"`
}

// Expr returns a BETWEEN expression over column without changing the receiver.
// It returns nil when either end is absent or does not parse, so a caller can
// skip the predicate entirely.
func (r *DateTimeRange) Expr(column field.Time) field.Expr {
	if r == nil || r.StartTime == "" || r.EndTime == "" {
		return nil
	}

	start, err := time.Parse(time.DateTime, r.StartTime)
	if err != nil {
		return nil
	}
	end, err := time.Parse(time.DateTime, r.EndTime)
	if err != nil {
		return nil
	}
	return column.Between(start, end)
}

// ToExpr returns the expression Expr would return, then clears StartTime and
// EndTime when an expression was produced. Callers that embed DateTimeRange in
// a request value object and later pass that object to GORM as a condition rely
// on the clearing so the range fields are not mistaken for columns. The fields
// are left in place when no expression is produced.
func (r *DateTimeRange) ToExpr(column field.Time) field.Expr {
	expression := r.Expr(column)
	if expression != nil && r != nil {
		r.StartTime = ""
		r.EndTime = ""
	}
	return expression
}
