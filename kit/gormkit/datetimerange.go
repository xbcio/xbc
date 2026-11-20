package gormkit

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
//
// The strings carry no zone, so they are interpreted in the process time.Local.
// That must match the loc setting of the database connection's DSN (loc=Local
// for a process-local connection), because the MySQL driver serializes a
// time.Time in that zone before sending it. Parsing in a different zone than
// the driver uses shifts the whole range by the offset between them: with a
// local zone of +08:00, a user's 19:38 would query from 03:38 the next day.
// Use ExprIn to state the zone explicitly.
func (r *DateTimeRange) Expr(column field.Time) field.Expr {
	return r.ExprIn(column, time.Local)
}

// ExprIn is Expr with an explicit zone for the zone-less range strings. It is
// meant for a connection whose DSN uses a loc other than the process local
// zone. A nil loc falls back to time.Local. It keeps Expr's other semantics:
// the receiver is not modified, and an absent or unparsable end yields nil.
func (r *DateTimeRange) ExprIn(column field.Time, loc *time.Location) field.Expr {
	if r == nil || r.StartTime == "" || r.EndTime == "" {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}

	start, err := time.ParseInLocation(time.DateTime, r.StartTime, loc)
	if err != nil {
		return nil
	}
	end, err := time.ParseInLocation(time.DateTime, r.EndTime, loc)
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
	return r.ToExprIn(column, time.Local)
}

// ToExprIn is ToExpr with an explicit zone, using the same nil fallback and
// value interpretation as ExprIn. It clears StartTime and EndTime only when an
// expression was produced, like ToExpr.
func (r *DateTimeRange) ToExprIn(column field.Time, loc *time.Location) field.Expr {
	expression := r.ExprIn(column, loc)
	if expression != nil && r != nil {
		r.StartTime = ""
		r.EndTime = ""
	}
	return expression
}
