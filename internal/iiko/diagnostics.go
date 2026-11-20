package iiko

import (
	"context"
	"fmt"
	"time"

	"greenroom-ai/internal/analytics"
)

type DiagnosticProbe struct {
	Stage                        string
	Status                       int
	Rows                         int
	DurationMS                   int64
	GroupFields, AggregateFields []string
	CorrelationID                string
	Error                        string
}

type diagnosticQuery struct {
	stage              string
	groups, aggregates []string
	applicationOptions bool
}

// DiagnoseSales runs at most eight narrow, read-only reports. It returns only
// technical response metadata; sales rows and order identifiers are discarded.
func (c *Client) DiagnoseSales(ctx context.Context, period analytics.Period) ([]DiagnosticProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	period.From, period.To = period.From.In(c.config.Location), period.To.In(c.config.Location)
	now := time.Now().In(c.config.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, c.config.Location)
	if period.Validate() != nil || !calendarBoundary(period.From, c.config.Location) || !period.To.Equal(period.From.AddDate(0, 0, 1)) || period.To.After(today) {
		return nil, ErrInvalidDiagnosticPeriod
	}
	if err := c.Check(ctx); err != nil {
		return nil, fmt.Errorf("diagnostic organization check: %w", err)
	}
	columns, err := c.Columns(ctx)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]Column, len(columns))
	for _, column := range columns {
		fields[column.Name] = column
	}
	for _, name := range []string{"UniqOrderId", "DishDiscountSumInt", "DishAmountInt"} {
		if !fields[name].CanAggregate {
			return nil, fmt.Errorf("diagnostic aggregate %s: %w", name, ErrMissingColumns)
		}
	}
	for _, name := range []string{"OpenDate.Typed", "DishGroup", "DishName"} {
		if !fields[name].CanGroup {
			return nil, fmt.Errorf("diagnostic group %s: %w", name, ErrMissingColumns)
		}
	}
	if !fields["OpenDate.Typed"].CanFilter {
		return nil, ErrMissingColumns
	}
	queries := []diagnosticQuery{
		{stage: "baseline", groups: []string{"OpenDate.Typed"}, aggregates: []string{"UniqOrderId", "DishDiscountSumInt"}},
		{stage: "quantity", groups: []string{"OpenDate.Typed"}, aggregates: []string{"UniqOrderId", "DishDiscountSumInt", "DishAmountInt"}},
		{stage: "menu_groups", groups: []string{"OpenDate.Typed", "DishGroup", "DishName"}, aggregates: []string{"UniqOrderId", "DishDiscountSumInt", "DishAmountInt"}},
	}
	groups := []string{"OpenDate.Typed", "DishGroup", "DishName"}
	productField := productIdentifierColumn(columns)
	if productField != "" {
		groups = append(groups, productField)
		queries = append(queries, diagnosticQuery{stage: "product_identity", groups: append([]string(nil), groups...), aggregates: []string{"UniqOrderId", "DishDiscountSumInt", "DishAmountInt"}})
	}
	orderField := orderIdentifierColumn(columns)
	if orderField != "" {
		groups = append(groups, orderField)
		queries = append(queries, diagnosticQuery{stage: "order_identity", groups: append([]string(nil), groups...), aggregates: []string{"UniqOrderId", "DishDiscountSumInt", "DishAmountInt"}})
	}
	queries = append(queries, diagnosticQuery{stage: "receipt_aggregate", groups: []string{}, aggregates: []string{"UniqOrderId"}})
	// Preserve the exact production grouping order for the final comparison.
	productionGroups := []string{"OpenDate.Typed", "DishGroup", "DishName"}
	if orderField != "" {
		productionGroups = append(productionGroups, orderField)
	}
	if productField != "" {
		productionGroups = append(productionGroups, productField)
	}
	queries = append(queries,
		diagnosticQuery{stage: "application_detail", groups: productionGroups, aggregates: []string{"DishAmountInt", "DishDiscountSumInt"}, applicationOptions: true},
		diagnosticQuery{stage: "application_receipts", groups: []string{}, aggregates: []string{"UniqOrderId"}, applicationOptions: true},
	)
	probes := make([]DiagnosticProbe, 0, len(queries))
	for _, query := range queries {
		if ctx.Err() != nil {
			return probes, ctx.Err()
		}
		probe := c.diagnosticProbe(ctx, period, query)
		probes = append(probes, probe)
		if query.stage == "baseline" && probe.Error != "" {
			break
		}
	}
	return probes, nil
}
func (c *Client) diagnosticProbe(ctx context.Context, period analytics.Period, query diagnosticQuery) DiagnosticProbe {
	filter := map[string]any{"name": "OpenDate.Typed", "filterType": "DateRange", "periodType": "CUSTOM", "from": period.From.Format("2006-01-02"), "to": period.To.Format("2006-01-02")}
	request := map[string]any{"reportType": "SALES", "aggregateFields": query.aggregates, "groupByRowFields": query.groups, "filters": []any{filter}}
	if query.applicationOptions {
		filter["includeLow"] = true
		filter["includeHigh"] = false
		request["buildSummary"] = false
	}
	c.logOlapRequest(query.stage, period, query.groups, query.aggregates)
	var response olapResponse
	started := time.Now()
	meta, err := c.postWithPolicy(ctx, "/api/reporting/v1/olap/get", request, &response, requestPolicy{attempts: 1, timeoutHeader: query.applicationOptions, stage: query.stage})
	result := DiagnosticProbe{Stage: query.stage, Status: meta.Status, Rows: len(response.Data), DurationMS: time.Since(started).Milliseconds(), GroupFields: append([]string{}, query.groups...), AggregateFields: append([]string{}, query.aggregates...), CorrelationID: meta.CorrelationID}
	if err == nil && response.Data == nil {
		err = ErrInvalidResponse
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
func (c *Client) logOlapRequest(stage string, period analytics.Period, groups, aggregates []string) {
	c.logger.Info("iiko OLAP request", "stage", stage, "date_from", period.From.In(c.config.Location).Format("2006-01-02"), "date_to_exclusive", period.To.In(c.config.Location).Format("2006-01-02"), "group_fields", groups, "aggregate_fields", aggregates)
}
