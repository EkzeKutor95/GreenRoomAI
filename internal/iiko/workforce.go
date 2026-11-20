package iiko

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"greenroom-ai/internal/analytics"
)

// WorkforceReport contains cashier-attributed sales and cash-register sessions
// observed in OLAP. It is neither the personnel directory nor personal shifts.
type WorkforceReport struct {
	Period     analytics.Period
	Role       string
	Employees  []SalesEmployee
	CashShifts []CashShift
	Warnings   []string
}
type SalesEmployee struct {
	ID, Name, Role    string
	Revenue, Quantity analytics.Decimal
	Receipts          int
}
type CashShift struct {
	ID, Number                        string
	Revenue, Quantity                 analytics.Decimal
	Receipts                          int
	FirstOrderOpened, LastOrderClosed *time.Time
}
type cachedWorkforce struct {
	report WorkforceReport
	until  time.Time
}
type orderAssignment struct {
	date, cashier, session, number string
	opened, closed                 *time.Time
}
type workforceAccumulator struct {
	employees                    map[string]SalesEmployee
	shifts                       map[string]CashShift
	orders                       map[string]orderAssignment
	missingOpened, missingClosed map[string]bool
	warnings                     map[string]struct{}
}

// Workforce performs a batch report, never one HTTP request per employee. Order
// identity is required so nonadditive per-item receipt counts are never summed.
func (c *Client) Workforce(ctx context.Context, period analytics.Period) (WorkforceReport, error) {
	if err := period.Validate(); err != nil {
		return WorkforceReport{}, err
	}
	period.From, period.To = period.From.In(c.config.Location), period.To.In(c.config.Location)
	if !calendarBoundary(period.From, c.config.Location) || !calendarBoundary(period.To, c.config.Location) {
		return WorkforceReport{}, ErrUnsupportedInterval
	}
	c.workforceMu.Lock()
	defer c.workforceMu.Unlock()
	key := period.From.Format(time.RFC3339) + "/" + period.To.Format(time.RFC3339)
	if cached, ok := c.workforceCache[key]; ok && time.Now().Before(cached.until) {
		return cloneWorkforce(cached.report), nil
	}
	if err := c.checkScope(ctx, false); err != nil {
		return WorkforceReport{}, err
	}
	columns, err := c.Columns(ctx)
	if err != nil {
		return WorkforceReport{}, err
	}
	byName := make(map[string]Column, len(columns))
	for _, column := range columns {
		byName[column.Name] = column
	}
	groups := []string{"OpenDate.Typed", "Cashier.Id", "Cashier", "SessionID", "SessionNum", "OpenTime", "CloseTime"}
	for _, name := range groups {
		if !byName[name].CanGroup {
			return WorkforceReport{}, fmt.Errorf("workforce grouping %s: %w", name, ErrMissingColumns)
		}
	}
	if !byName["OpenDate.Typed"].CanFilter {
		return WorkforceReport{}, ErrMissingColumns
	}
	for _, name := range []string{"DishAmountInt", "DishDiscountSumInt", "UniqOrderId"} {
		if !byName[name].CanAggregate {
			return WorkforceReport{}, fmt.Errorf("workforce aggregate %s: %w", name, ErrMissingColumns)
		}
	}
	orderField := orderIdentifierColumn(columns)
	if orderField == "" {
		return WorkforceReport{}, fmt.Errorf("workforce needs concrete order identity: %w", ErrMissingColumns)
	}
	groups = append(groups, orderField)
	acc := workforceAccumulator{employees: make(map[string]SalesEmployee), shifts: make(map[string]CashShift), orders: make(map[string]orderAssignment), missingOpened: make(map[string]bool), missingClosed: make(map[string]bool), warnings: make(map[string]struct{})}
	rows := 0
	for start := period.From; start.Before(period.To); {
		end := start.AddDate(0, 0, 30)
		if end.After(period.To) {
			end = period.To
		}
		chunk := analytics.Period{From: start, To: end}
		response, err := c.olap(ctx, chunk, groups, []string{"DishAmountInt", "DishDiscountSumInt", "UniqOrderId"}, "workforce_orders")
		if err != nil {
			return WorkforceReport{}, err
		}
		rows += len(response.Data)
		if rows > 250000 {
			return WorkforceReport{}, ErrResultLimit
		}
		for _, raw := range response.Data {
			if err := acc.add(raw, orderField, chunk, c.config.Location); err != nil {
				return WorkforceReport{}, err
			}
		}
		start = end
	}
	report := WorkforceReport{Period: period, Role: "Кассир", Employees: make([]SalesEmployee, 0, len(acc.employees)), CashShifts: make([]CashShift, 0, len(acc.shifts)), Warnings: []string{
		"Источник: OLAP, роль Cashier (кассир чека). Это сотрудники с продажами за выбранный период, а не полный кадровый справочник.",
		"Суммы кассовых смен ограничены выбранными операционными днями OpenDate.Typed; смена может продолжаться за пределами периода.",
		"Время первого открытия и последнего закрытия заказа не является временем открытия или закрытия кассовой либо личной смены.",
		"Суммы и дробные количества OLAP сохранены со знаком; правила возвратов и удалений требуют сверки с iikoOffice.",
	}}
	for _, employee := range acc.employees {
		report.Employees = append(report.Employees, employee)
	}
	for id, shift := range acc.shifts {
		if acc.missingOpened[id] {
			shift.FirstOrderOpened = nil
		}
		if acc.missingClosed[id] {
			shift.LastOrderClosed = nil
		}
		report.CashShifts = append(report.CashShifts, shift)
	}
	additionalWarnings := make([]string, 0, len(acc.warnings))
	for warning := range acc.warnings {
		additionalWarnings = append(additionalWarnings, warning)
	}
	sort.Strings(additionalWarnings)
	report.Warnings = append(report.Warnings, additionalWarnings...)
	sort.Slice(report.Employees, func(i, j int) bool {
		if cmp := report.Employees[i].Revenue.Cmp(report.Employees[j].Revenue); cmp != 0 {
			return cmp > 0
		}
		return report.Employees[i].ID < report.Employees[j].ID
	})
	sort.Slice(report.CashShifts, func(i, j int) bool {
		a, b := report.CashShifts[i], report.CashShifts[j]
		if (a.FirstOrderOpened == nil) != (b.FirstOrderOpened == nil) {
			return a.FirstOrderOpened != nil
		}
		if a.FirstOrderOpened != nil && !a.FirstOrderOpened.Equal(*b.FirstOrderOpened) {
			return a.FirstOrderOpened.After(*b.FirstOrderOpened)
		}
		return a.ID < b.ID
	})
	retained := 0
	now := time.Now()
	for cacheKey, cached := range c.workforceCache {
		if now.After(cached.until) {
			delete(c.workforceCache, cacheKey)
		} else {
			retained += len(cached.report.Employees) + len(cached.report.CashShifts)
		}
	}
	size := len(report.Employees) + len(report.CashShifts)
	if len(c.workforceCache) >= 16 || retained+size > 10000 {
		clear(c.workforceCache)
	}
	if size <= 10000 {
		c.workforceCache[key] = cachedWorkforce{report: cloneWorkforce(report), until: now.Add(time.Minute)}
	}
	return report, nil
}

func (a *workforceAccumulator) add(raw map[string]json.RawMessage, orderField string, period analytics.Period, loc *time.Location) error {
	dateText, err := stringField(raw, "OpenDate.Typed", true)
	if err != nil {
		return err
	}
	date, err := parseOlapDate(dateText, loc)
	if err != nil || date.Before(period.From) || !date.Before(period.To) {
		return fmt.Errorf("workforce accounting date: %w", ErrInvalidResponse)
	}
	orderID, err := stringField(raw, orderField, true)
	if err != nil {
		return err
	}
	cashierID, err := stringField(raw, "Cashier.Id", false)
	if err != nil {
		return err
	}
	name, err := stringField(raw, "Cashier", false)
	if err != nil {
		return err
	}
	if cashierID == "" {
		name = "Кассир не указан"
		a.warnings["Есть чеки без ID кассира: они учтены отдельной строкой и не используются как идентификатор сотрудника."] = struct{}{}
	} else if name == "" {
		name = "Имя кассира не указано"
		a.warnings["Для части ID кассиров OLAP не предоставила имя."] = struct{}{}
	}
	sessionID, err := stringField(raw, "SessionID", true)
	if err != nil {
		return err
	}
	number, err := sessionNumber(raw["SessionNum"])
	if err != nil {
		return err
	}
	opened, err := optionalOlapTimestamp(raw, "OpenTime", loc)
	if err != nil {
		return err
	}
	closed, err := optionalOlapTimestamp(raw, "CloseTime", loc)
	if err != nil {
		return err
	}
	if opened != nil && closed != nil && closed.Before(*opened) {
		return fmt.Errorf("order closure precedes opening: %w", ErrInvalidResponse)
	}
	count, err := receiptTotal([]map[string]json.RawMessage{raw})
	if err != nil || count != 1 {
		return fmt.Errorf("concrete order count must be one: %w", ErrInvalidResponse)
	}
	var revenue, quantity analytics.Decimal
	if json.Unmarshal(raw["DishDiscountSumInt"], &revenue) != nil || json.Unmarshal(raw["DishAmountInt"], &quantity) != nil {
		return fmt.Errorf("workforce monetary or quantity value: %w", ErrInvalidResponse)
	}
	assignment := orderAssignment{date: date.Format("2006-01-02"), cashier: cashierID, session: sessionID, number: number, opened: opened, closed: closed}
	previous, seen := a.orders[orderID]
	if seen && (previous.date != assignment.date || previous.cashier != cashierID || previous.session != sessionID || previous.number != number || !sameOptionalTime(previous.opened, opened) || !sameOptionalTime(previous.closed, closed)) {
		return fmt.Errorf("one order has conflicting cashier/session/time assignments: %w", ErrInvalidResponse)
	}
	// Every dimension is grouped together with concrete order identity; an
	// identical repeated order row cannot be a separate item-level contribution.
	if seen {
		return fmt.Errorf("OLAP repeated a concrete order grouping: %w", ErrInvalidResponse)
	}
	employee := a.employees[cashierID]
	if employee.Name != "" && employee.Name != name {
		return fmt.Errorf("cashier identity has conflicting names: %w", ErrInvalidResponse)
	}
	employee.ID, employee.Name, employee.Role = cashierID, name, "Кассир"
	employee.Revenue = employee.Revenue.Add(revenue)
	employee.Quantity = employee.Quantity.Add(quantity)
	shift := a.shifts[sessionID]
	if shift.Number != "" && shift.Number != number {
		return fmt.Errorf("cash session identity has conflicting numbers: %w", ErrInvalidResponse)
	}
	shift.ID, shift.Number = sessionID, number
	shift.Revenue = shift.Revenue.Add(revenue)
	shift.Quantity = shift.Quantity.Add(quantity)
	if !seen {
		employee.Receipts++
		shift.Receipts++
		a.orders[orderID] = assignment
	}
	if opened == nil {
		a.missingOpened[sessionID] = true
		a.warnings["Время открытия части заказов отсутствует; первая отметка заказа такой кассовой смены недоступна."] = struct{}{}
	} else if shift.FirstOrderOpened == nil || opened.Before(*shift.FirstOrderOpened) {
		shift.FirstOrderOpened = opened
	}
	if closed == nil {
		a.missingClosed[sessionID] = true
		a.warnings["Время закрытия части заказов отсутствует; последняя отметка заказа такой кассовой смены недоступна."] = struct{}{}
	} else if shift.LastOrderClosed == nil || closed.After(*shift.LastOrderClosed) {
		shift.LastOrderClosed = closed
	}
	a.employees[cashierID] = employee
	a.shifts[sessionID] = shift
	return nil
}
func sessionNumber(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var number json.Number
		if json.Unmarshal(raw, &number) != nil {
			return "", fmt.Errorf("cash session number: %w", ErrInvalidResponse)
		}
		text = number.String()
	}
	if _, err := strconv.ParseUint(text, 10, 64); err != nil {
		return "", fmt.Errorf("cash session number: %w", ErrInvalidResponse)
	}
	return text, nil
}
func optionalOlapTimestamp(raw map[string]json.RawMessage, name string, loc *time.Location) (*time.Time, error) {
	value, err := stringField(raw, name, false)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return &parsed, nil
	}
	// OLAP DATETIME values in the verified response are local ISO timestamps;
	// the venue timezone is explicit configuration, unlike UTC webhook times.
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			return &parsed, nil
		}
	}
	return nil, fmt.Errorf("OLAP timestamp %s: %w", name, ErrInvalidResponse)
}
func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
func cloneWorkforce(report WorkforceReport) WorkforceReport {
	report.Employees = append([]SalesEmployee{}, report.Employees...)
	report.CashShifts = append([]CashShift{}, report.CashShifts...)
	report.Warnings = append([]string{}, report.Warnings...)
	for i := range report.CashShifts {
		if source := report.CashShifts[i].FirstOrderOpened; source != nil {
			value := *source
			report.CashShifts[i].FirstOrderOpened = &value
		}
		if source := report.CashShifts[i].LastOrderClosed; source != nil {
			value := *source
			report.CashShifts[i].LastOrderClosed = &value
		}
	}
	return report
}
