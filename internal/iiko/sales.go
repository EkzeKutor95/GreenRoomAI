package iiko

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"greenroom-ai/internal/analytics"
)

type Column struct {
	Name         string `json:"name"`
	DisplayName  string `json:"displayName"`
	Type         string `json:"type"`
	CanAggregate bool   `json:"canAggregate"`
	CanFilter    bool   `json:"canFilter"`
	CanGroup     bool   `json:"canGroup"`
}

type cachedSales struct {
	data  analytics.Dataset
	until time.Time
}

type olapResponse struct {
	Data []map[string]json.RawMessage `json:"data"`
}

func (c *Client) Columns(ctx context.Context) ([]Column, error) {
	c.metadataMu.Lock()
	defer c.metadataMu.Unlock()
	if time.Now().Before(c.columnsUntil) {
		return append([]Column(nil), c.columns...), nil
	}
	var columns []Column
	if err := c.post(ctx, "/api/reporting/v1/olap/columns/get", map[string]string{"reportType": "SALES"}, &columns); err != nil {
		return nil, fmt.Errorf("load OLAP metadata: %w", err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("empty metadata: %w", ErrMissingColumns)
	}
	c.columns = columns
	c.columnsUntil = time.Now().Add(time.Hour)
	return append([]Column(nil), columns...), nil
}

func (c *Client) Sales(ctx context.Context, p analytics.Period) (analytics.Dataset, error) {
	if err := p.Validate(); err != nil {
		return analytics.Dataset{}, err
	}
	// Converting to the configured zone preserves instants and makes all date
	// boundaries and the API's calendar labels use the same venue timezone.
	p.From, p.To = p.From.In(c.config.Location), p.To.In(c.config.Location)
	// The verified Cloud OLAP contract accepts calendar dates only. Widening an
	// employee's interval to a whole day would mislabel establishment revenue.
	if !calendarBoundary(p.From, c.config.Location) || !calendarBoundary(p.To, c.config.Location) {
		return analytics.Dataset{}, ErrUnsupportedInterval
	}
	c.salesMu.Lock()
	defer c.salesMu.Unlock()
	key := p.From.Format(time.RFC3339) + "/" + p.To.Format(time.RFC3339)
	if entry, ok := c.salesCache[key]; ok && time.Now().Before(entry.until) {
		return cloneDataset(entry.data), nil
	}
	if err := c.checkScope(ctx, false); err != nil {
		return analytics.Dataset{}, err
	}
	columns, err := c.Columns(ctx)
	if err != nil {
		return analytics.Dataset{}, err
	}
	byName := make(map[string]Column, len(columns))
	for _, column := range columns {
		byName[column.Name] = column
	}
	for _, field := range []string{"OpenDate.Typed", "DishGroup", "DishName"} {
		if !byName[field].CanGroup {
			return analytics.Dataset{}, fmt.Errorf("grouping %s: %w", field, ErrMissingColumns)
		}
	}
	for _, field := range []string{"DishAmountInt", "DishDiscountSumInt"} {
		if !byName[field].CanAggregate {
			return analytics.Dataset{}, fmt.Errorf("aggregate %s: %w", field, ErrMissingColumns)
		}
	}
	if !byName["OpenDate.Typed"].CanFilter {
		return analytics.Dataset{}, fmt.Errorf("date filter: %w", ErrMissingColumns)
	}
	// A field is used only when the live metadata explicitly exposes it with
	// grouping capability. UniqOrderId is an aggregate count, never an order ID.
	orderField := orderIdentifierColumn(columns)
	productField := productIdentifierColumn(columns)
	groups := []string{"OpenDate.Typed", "DishGroup", "DishName"}
	if orderField != "" {
		groups = append(groups, orderField)
	}
	if productField != "" {
		groups = append(groups, productField)
	}
	dataset := analytics.Dataset{OrderIDsAvailable: orderField != "", Warnings: []string{"Выручка — DishDiscountSumInt из OLAP (после скидок); знаки сумм и количества сохранены. Правила исключения удалённых позиций и возвратов требуют сверки с iikoOffice."}}
	if productField == "" {
		dataset.Warnings = append(dataset.Warnings, "Идентификаторы товаров недоступны: десерты определяются по группам меню.")
	}
	totalReceipts := 0
	receiptsAvailable := byName["UniqOrderId"].CanAggregate
	// Each order belongs to its OpenDate.Typed date, so non-overlapping date
	// chunks partition orders. 30-day chunks also satisfy the 31-day API limit.
	for start := p.From; start.Before(p.To); {
		end := start.AddDate(0, 0, 30)
		if end.After(p.To) {
			end = p.To
		}
		chunk := analytics.Period{From: start, To: end}
		response, err := c.olap(ctx, chunk, groups, []string{"DishAmountInt", "DishDiscountSumInt"}, "sales_detail")
		if err != nil {
			return analytics.Dataset{}, err
		}
		rows, err := decodeRows(response.Data, chunk, orderField, productField)
		if err != nil {
			return analytics.Dataset{}, err
		}
		dataset.Rows = append(dataset.Rows, rows...)
		if len(dataset.Rows) > 250000 {
			return analytics.Dataset{}, ErrResultLimit
		}
		if receiptsAvailable {
			totals, err := c.olap(ctx, chunk, []string{}, []string{"UniqOrderId"}, "receipt_totals")
			if err != nil {
				return analytics.Dataset{}, err
			}
			count, err := receiptTotal(totals.Data)
			if err != nil {
				return analytics.Dataset{}, err
			}
			totalReceipts += count
		}
		start = end
	}
	if receiptsAvailable {
		if (totalReceipts == 0 && len(dataset.Rows) > 0) || (totalReceipts > 0 && len(dataset.Rows) == 0) {
			return analytics.Dataset{}, fmt.Errorf("receipt aggregate disagrees with sales rows: %w", ErrInvalidResponse)
		}
		if orderField != "" {
			identities := make(map[string]struct{})
			for _, row := range dataset.Rows {
				identities[row.OrderID] = struct{}{}
			}
			if len(identities) != totalReceipts {
				return analytics.Dataset{}, fmt.Errorf("receipt aggregate disagrees with order identities: %w", ErrInvalidResponse)
			}
		}
		dataset.ReceiptCount = &totalReceipts
	}
	if orderField == "" {
		dataset.Warnings = append(dataset.Warnings, "Нет подтверждённого группируемого ID заказа: чеки с десертами и их доля недоступны; UniqOrderId используется только в отдельном отчёте без товарных группировок.")
	}
	now := time.Now()
	retainedRows := 0
	for cacheKey, entry := range c.salesCache {
		if now.After(entry.until) {
			delete(c.salesCache, cacheKey)
		} else {
			retainedRows += len(entry.data.Rows)
		}
	}
	const maxCachedRows = 50000
	if len(c.salesCache) >= 16 || retainedRows+len(dataset.Rows) > maxCachedRows {
		clear(c.salesCache)
	}
	if len(dataset.Rows) <= maxCachedRows {
		c.salesCache[key] = cachedSales{data: cloneDataset(dataset), until: now.Add(time.Minute)}
	}
	return dataset, nil
}
func cloneDataset(d analytics.Dataset) analytics.Dataset {
	d.Rows = append([]analytics.Row(nil), d.Rows...)
	d.Warnings = append([]string(nil), d.Warnings...)
	if d.ReceiptCount != nil {
		count := *d.ReceiptCount
		d.ReceiptCount = &count
	}
	return d
}
func calendarBoundary(t time.Time, loc *time.Location) bool {
	v := t.In(loc)
	return v.Hour() == 0 && v.Minute() == 0 && v.Second() == 0 && v.Nanosecond() == 0
}
func (c *Client) olap(ctx context.Context, p analytics.Period, groups, aggregates []string, stage string) (olapResponse, error) {
	c.logOlapRequest(stage, p, groups, aggregates)
	request := map[string]any{"reportType": "SALES", "aggregateFields": aggregates, "groupByRowFields": groups, "buildSummary": false, "filters": []any{map[string]any{"name": "OpenDate.Typed", "filterType": "DateRange", "periodType": "CUSTOM", "from": p.From.In(c.config.Location).Format("2006-01-02"), "to": p.To.In(c.config.Location).Format("2006-01-02"), "includeLow": true, "includeHigh": false}}}
	var response olapResponse
	policy := normalRequestPolicy()
	policy.stage = stage
	if _, err := c.postWithPolicy(ctx, "/api/reporting/v1/olap/get", request, &response, policy); err != nil {
		return response, fmt.Errorf("run OLAP %s: %w", stage, err)
	}
	if response.Data == nil {
		return response, fmt.Errorf("missing OLAP data array: %w", ErrInvalidResponse)
	}
	return response, nil
}
func decodeRows(data []map[string]json.RawMessage, p analytics.Period, orderField, productField string) ([]analytics.Row, error) {
	rows := make([]analytics.Row, 0, len(data))
	for _, raw := range data {
		var row analytics.Row
		var err error
		if row.Date, err = stringField(raw, "OpenDate.Typed", true); err != nil {
			return nil, err
		}
		date, err := parseOlapDate(row.Date, p.From.Location())
		if err != nil || date.Before(p.From) || !date.Before(p.To) {
			return nil, fmt.Errorf("OLAP row date: %w", ErrInvalidResponse)
		}
		row.Date = date.Format("2006-01-02")
		if row.Group, err = stringField(raw, "DishGroup", false); err != nil {
			return nil, err
		}
		if row.Name, err = stringField(raw, "DishName", true); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw["DishAmountInt"], &row.Quantity); err != nil {
			return nil, fmt.Errorf("OLAP quantity: %w", ErrInvalidResponse)
		}
		if err = json.Unmarshal(raw["DishDiscountSumInt"], &row.Revenue); err != nil {
			return nil, fmt.Errorf("OLAP revenue: %w", ErrInvalidResponse)
		}
		if orderField != "" {
			if row.OrderID, err = stringField(raw, orderField, true); err != nil {
				return nil, err
			}
		}
		if productField != "" {
			if row.ProductID, err = stringField(raw, productField, true); err != nil {
				return nil, err
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func stringField(raw map[string]json.RawMessage, name string, required bool) (string, error) {
	value, ok := raw[name]
	if !ok {
		return "", fmt.Errorf("missing OLAP field %s: %w", name, ErrInvalidResponse)
	}
	if string(value) == "null" && !required {
		return "", nil
	}
	var result string
	if json.Unmarshal(value, &result) != nil || (required && result == "") {
		return "", fmt.Errorf("OLAP field %s: %w", name, ErrInvalidResponse)
	}
	return result, nil
}
func receiptTotal(data []map[string]json.RawMessage) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) != 1 {
		return 0, fmt.Errorf("unexpected receipt grouping: %w", ErrInvalidResponse)
	}
	var count analytics.Decimal
	if err := json.Unmarshal(data[0]["UniqOrderId"], &count); err != nil {
		return 0, fmt.Errorf("receipt count: %w", ErrInvalidResponse)
	}
	value, err := strconv.Atoi(count.String())
	if err != nil || value < 0 {
		return 0, fmt.Errorf("receipt count: %w", ErrInvalidResponse)
	}
	return value, nil
}

// Column names are supplied by the current official metadata. An aggregate
// count or a receipt number cannot establish the identity of a concrete order.
func orderIdentifierColumn(columns []Column) string {
	for _, column := range columns {
		if !column.CanGroup || !strings.EqualFold(column.Type, "ID") || column.Name == "UniqOrderId" {
			continue
		}
		switch strings.ToLower(column.DisplayName) {
		case "order id", "unique order id", "id заказа", "идентификатор заказа", "уникальный идентификатор заказа", "уникальный id заказа":
			return column.Name
		}
	}
	return ""
}

func parseOlapDate(value string, loc *time.Location) (time.Time, error) {
	if len(value) == 10 {
		return time.ParseInLocation("2006-01-02", value, loc)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
		parsed, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		if parsed.Hour() != 0 || parsed.Minute() != 0 || parsed.Second() != 0 || parsed.Nanosecond() != 0 {
			return time.Time{}, ErrInvalidResponse
		}
		// OpenDate.Typed is an accounting-date label, including when serialized
		// with a midnight time. Preserve its calendar date in the venue timezone.
		return time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, loc), nil
	}
	return time.Time{}, ErrInvalidResponse
}

func productIdentifierColumn(columns []Column) string {
	for _, column := range columns {
		if column.Name == "DishId" && column.CanGroup && (strings.EqualFold(column.Type, "ID") || strings.EqualFold(column.Type, "STRING")) {
			return column.Name
		}
	}
	return ""
}
