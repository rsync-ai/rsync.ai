package handlers

// chart_grain.go writes a date grain for each engine (dashboards-design.md § 14.3): the
// start of the day, week, month, quarter or year a value falls in. A week starts on the
// connection's first day of the week and a quarter or a year in its fiscal year's first
// month (decision 14), never on the engine's or the session's default, which is Sunday on
// BigQuery, ClickHouse and SQL Server in us_english. MySQL, MariaDB and SQL Server before
// 2022 have no DATE_TRUNC, so theirs is date arithmetic.
//
// A week starting on day W (1 Monday … 7 Sunday) is the ISO week of the value moved k =
// (8-W)%7 days later, moved back k days; a fiscal quarter or year starting in month F is
// the calendar one of the value moved n = F-1 months earlier, moved back n months.

import (
	"fmt"

	"api-gateway/internal/charts"
)

var weekdayNames = [...]string{"MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}

// chartDateGrain is expr, a quoted source column, truncated to grain on dialect.
func chartDateGrain(dialect, grain, expr string, weekStart, fiscalMonth int) string {
	if weekStart < 1 || weekStart > 7 {
		weekStart = 1
	}
	if fiscalMonth < 1 || fiscalMonth > 12 {
		fiscalMonth = 1
	}
	k := (8 - weekStart) % 7 // days a week start moves to Monday
	n := fiscalMonth - 1     // months a fiscal year start moves to January
	e := expr
	switch dialect {
	case "postgres":
		switch grain {
		case charts.GrainDay:
			return "DATE_TRUNC('day', " + e + ")"
		case charts.GrainWeek:
			if k == 0 {
				return "DATE_TRUNC('week', " + e + ")"
			}
			return fmt.Sprintf("DATE_TRUNC('week', %s + INTERVAL '%d days') - INTERVAL '%d days'", e, k, k)
		case charts.GrainMonth:
			return "DATE_TRUNC('month', " + e + ")"
		default: // quarter, year
			if n == 0 {
				return "DATE_TRUNC('" + grain + "', " + e + ")"
			}
			return fmt.Sprintf("DATE_TRUNC('%s', %s - INTERVAL '%d months') + INTERVAL '%d months'", grain, e, n, n)
		}
	case "mysql":
		switch grain {
		case charts.GrainDay:
			return "DATE(" + e + ")"
		case charts.GrainWeek:
			// WEEKDAY is 0 on Monday; the days since the week started.
			since := "WEEKDAY(" + e + ")"
			if k != 0 {
				since = fmt.Sprintf("MOD(WEEKDAY(%s) + %d, 7)", e, k)
			}
			return fmt.Sprintf("DATE_SUB(DATE(%s), INTERVAL %s DAY)", e, since)
		case charts.GrainMonth:
			return fmt.Sprintf("MAKEDATE(YEAR(%s), 1) + INTERVAL (MONTH(%s) - 1) MONTH", e, e)
		default:
			s := e
			if n != 0 {
				s = fmt.Sprintf("(%s - INTERVAL %d MONTH)", e, n)
			}
			out := fmt.Sprintf("MAKEDATE(YEAR(%s), 1)", s)
			if grain == charts.GrainQuarter {
				out = fmt.Sprintf("%s + INTERVAL (QUARTER(%s) - 1) QUARTER", out, s)
			}
			if n != 0 {
				out = fmt.Sprintf("(%s) + INTERVAL %d MONTH", out, n)
			}
			return out
		}
	case "sqlserver":
		switch grain {
		case charts.GrainDay:
			return "CAST(" + e + " AS date)"
		case charts.GrainWeek:
			// 1900-01-01 was a Monday, so 1900-01-0W is a day W; the days since the
			// week started, whatever DATEFIRST is, and right before 1900 too.
			return fmt.Sprintf("DATEADD(day, -((DATEDIFF(day, '1900010%d', %s) %% 7 + 7) %% 7), CAST(%s AS date))", weekStart, e, e)
		case charts.GrainMonth:
			return fmt.Sprintf("DATEFROMPARTS(YEAR(%s), MONTH(%s), 1)", e, e)
		default:
			s := e
			if n != 0 {
				s = fmt.Sprintf("DATEADD(month, -%d, %s)", n, e)
			}
			out := fmt.Sprintf("DATEFROMPARTS(YEAR(%s), 1, 1)", s)
			if grain == charts.GrainQuarter {
				out = fmt.Sprintf("DATEFROMPARTS(YEAR(%s), (DATEPART(quarter, %s) - 1) * 3 + 1, 1)", s, s)
			}
			if n != 0 {
				out = fmt.Sprintf("DATEADD(month, %d, %s)", n, out)
			}
			return out
		}
	case "bigquery":
		dt := "DATE(" + e + ")"
		switch grain {
		case charts.GrainDay:
			return dt
		case charts.GrainWeek:
			return fmt.Sprintf("DATE_TRUNC(%s, WEEK(%s))", dt, weekdayNames[weekStart-1])
		case charts.GrainMonth:
			return "DATE_TRUNC(" + dt + ", MONTH)"
		default:
			unit := "QUARTER"
			if grain == charts.GrainYear {
				unit = "YEAR"
			}
			if n == 0 {
				return fmt.Sprintf("DATE_TRUNC(%s, %s)", dt, unit)
			}
			return fmt.Sprintf("DATE_ADD(DATE_TRUNC(DATE_SUB(%s, INTERVAL %d MONTH), %s), INTERVAL %d MONTH)", dt, n, unit, n)
		}
	case "snowflake":
		switch grain {
		case charts.GrainDay:
			return "DATE_TRUNC('DAY', " + e + ")"
		case charts.GrainWeek:
			// DAYOFWEEKISO is 1 on Monday whatever WEEK_START is; the days since the
			// week started.
			return fmt.Sprintf("DATEADD('DAY', -MOD(DAYOFWEEKISO(%s) + %d, 7), DATE_TRUNC('DAY', %s))", e, 7-weekStart, e)
		case charts.GrainMonth:
			return "DATE_TRUNC('MONTH', " + e + ")"
		default:
			unit := "QUARTER"
			if grain == charts.GrainYear {
				unit = "YEAR"
			}
			if n == 0 {
				return fmt.Sprintf("DATE_TRUNC('%s', %s)", unit, e)
			}
			return fmt.Sprintf("DATEADD('MONTH', %d, DATE_TRUNC('%s', DATEADD('MONTH', -%d, %s)))", n, unit, n, e)
		}
	case "databricks":
		switch grain {
		case charts.GrainDay:
			return "DATE_TRUNC('DAY', " + e + ")"
		case charts.GrainWeek:
			if k == 0 {
				return "DATE_TRUNC('WEEK', " + e + ")"
			}
			return fmt.Sprintf("DATE_TRUNC('WEEK', %s + INTERVAL %d DAYS) - INTERVAL %d DAYS", e, k, k)
		case charts.GrainMonth:
			return "DATE_TRUNC('MONTH', " + e + ")"
		default:
			unit := "QUARTER"
			if grain == charts.GrainYear {
				unit = "YEAR"
			}
			if n == 0 {
				return fmt.Sprintf("DATE_TRUNC('%s', %s)", unit, e)
			}
			return fmt.Sprintf("ADD_MONTHS(DATE_TRUNC('%s', ADD_MONTHS(%s, -%d)), %d)", unit, e, n, n)
		}
	case "clickhouse":
		switch grain {
		case charts.GrainDay:
			return "toDate(" + e + ")"
		case charts.GrainWeek:
			// toDayOfWeek is 1 on Monday; the days since the week started.
			return fmt.Sprintf("toDate(%s) - ((toDayOfWeek(%s) + %d) %% 7)", e, e, 7-weekStart)
		case charts.GrainMonth:
			return "toStartOfMonth(" + e + ")"
		default:
			fn := "toStartOfQuarter"
			if grain == charts.GrainYear {
				fn = "toStartOfYear"
			}
			if n == 0 {
				return fn + "(" + e + ")"
			}
			return fmt.Sprintf("addMonths(%s(subtractMonths(toDate(%s), %d)), %d)", fn, e, n, n)
		}
	case "oracle":
		switch grain {
		case charts.GrainDay:
			return "TRUNC(" + e + ", 'DD')"
		case charts.GrainWeek:
			if k == 0 {
				return "TRUNC(" + e + ", 'IW')"
			}
			return fmt.Sprintf("TRUNC(%s + %d, 'IW') - %d", e, k, k)
		case charts.GrainMonth:
			return "TRUNC(" + e + ", 'MM')"
		default:
			unit := "Q"
			if grain == charts.GrainYear {
				unit = "YYYY"
			}
			if n == 0 {
				return fmt.Sprintf("TRUNC(%s, '%s')", e, unit)
			}
			return fmt.Sprintf("ADD_MONTHS(TRUNC(ADD_MONTHS(%s, -%d), '%s'), %d)", e, n, unit, n)
		}
	}
	return e
}
