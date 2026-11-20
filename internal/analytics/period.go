package analytics

import (
	"fmt"
	"time"
)

// Period is a half-open interval in the venue's timezone: [From, To).
type Period struct{ From, To time.Time }

func (p Period) Validate() error {
	if p.From.IsZero() || p.To.IsZero() || !p.From.Before(p.To) || p.To.After(p.From.AddDate(0, 0, 366)) {
		return ErrInvalidPeriod
	}
	return nil
}
func (p Period) String() string {
	return p.From.Format("02.01.2006 15:04") + " — " + p.To.Format("02.01.2006 15:04") + " (конец не включён)"
}
func NewPeriod(start, end string, loc *time.Location) (Period, error) {
	if loc == nil {
		return Period{}, ErrInvalidPeriod
	}
	from, err := time.ParseInLocation("2006-01-02", start, loc)
	if err != nil {
		return Period{}, fmt.Errorf("parse period start: %w", ErrInvalidPeriod)
	}
	to, err := time.ParseInLocation("2006-01-02", end, loc)
	if err != nil {
		return Period{}, fmt.Errorf("parse period end: %w", ErrInvalidPeriod)
	}
	p := Period{From: from, To: to.AddDate(0, 0, 1)}
	return p, p.Validate()
}
func Preset(name string, now time.Time, loc *time.Location) (Period, error) {
	if loc == nil {
		return Period{}, ErrInvalidPeriod
	}
	now = now.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	p := Period{To: today.AddDate(0, 0, 1)}
	switch name {
	case "today":
		p.From = today
	case "yesterday":
		p.From = today.AddDate(0, 0, -1)
		p.To = today
	case "7d":
		p.From = today.AddDate(0, 0, -6)
	case "10d":
		p.From = today.AddDate(0, 0, -9)
	case "30d":
		p.From = today.AddDate(0, 0, -29)
	case "month":
		p.From = month
	case "previous_month":
		p.From = month.AddDate(0, -1, 0)
		p.To = month
	default:
		return Period{}, ErrInvalidPeriod
	}
	return p, p.Validate()
}
func ComparablePeriods(name string, now time.Time, loc *time.Location) (Period, Period, error) {
	if loc == nil {
		return Period{}, Period{}, ErrInvalidPeriod
	}
	now = now.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	var current, previous Period
	switch name {
	case "7d", "30d":
		days := 7
		if name == "30d" {
			days = 30
		}
		current = Period{From: today.AddDate(0, 0, -days), To: today}
		previous = Period{From: current.From.AddDate(0, 0, -days), To: current.From}
	case "month":
		current = Period{From: month, To: today.AddDate(0, 0, 1)}
		previous = Period{From: month.AddDate(0, -1, 0), To: month}
	case "month_to_date":
		// The verified OLAP API accepts dates, so compare completed calendar
		// days only. An unfinished day cannot be matched to intraday history.
		current = Period{From: month, To: today}
		start := month.AddDate(0, -1, 0)
		end := start.AddDate(0, 0, now.Day()-1)
		if end.After(month) {
			end = month
		}
		previous = Period{From: start, To: end}
	default:
		return Period{}, Period{}, ErrInvalidPeriod
	}
	if err := current.Validate(); err != nil {
		return Period{}, Period{}, err
	}
	if err := previous.Validate(); err != nil {
		return Period{}, Period{}, err
	}
	return current, previous, nil
}

// ScheduledShift is a scheduled interval, never evidence of a personal shift.
// Evening work that ends after midnight belongs to the date of its start.
func ScheduledShift(date time.Time, evening bool, loc *time.Location) Period {
	date = date.In(loc)
	start := time.Date(date.Year(), date.Month(), date.Day(), 7, 0, 0, 0, loc)
	end := time.Date(date.Year(), date.Month(), date.Day(), 18, 0, 0, 0, loc)
	if evening {
		start = end
		end = time.Date(date.Year(), date.Month(), date.Day()+1, 1, 0, 0, 0, loc)
	}
	return Period{From: start, To: end}
}
