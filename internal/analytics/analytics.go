package analytics

import (
	"fmt"
	"sort"
	"time"
)

type Row struct {
	Date, Group, Name, ProductID, OrderID string
	Quantity, Revenue                     Decimal
}
type Dataset struct {
	Rows              []Row
	ReceiptCount      *int
	OrderIDsAvailable bool
	Warnings          []string
}
type DessertRules struct{ Groups, ProductIDs []string }
type Entry struct {
	ID, Name, Group   string
	Quantity, Revenue Decimal
}
type Report struct {
	Period                                  Period
	Revenue, Quantity, Average              Decimal
	Receipts                                *int
	DessertReceipts, WithoutDessertReceipts *int
	DessertQuantity, DessertRevenue         Decimal
	DessertShare                            *Decimal
	Items, Groups, Daily                    []Entry
	Rows                                    []Row
	Warnings                                []string
}

// Aggregate retains signed quantities and revenue supplied by iiko. Dessert
// penetration means a distinct order with positive net dessert quantity after
// returns. UniqOrderId counts grouped by dishes cannot establish that metric.
func Aggregate(period Period, dataset Dataset, rules DessertRules) (Report, error) {
	if err := period.Validate(); err != nil {
		return Report{}, err
	}
	if dataset.ReceiptCount != nil && *dataset.ReceiptCount < 0 {
		return Report{}, fmt.Errorf("negative receipt count: %w", ErrInvalidDataset)
	}
	report := Report{Period: period, Rows: append([]Row(nil), dataset.Rows...), Warnings: append([]string(nil), dataset.Warnings...)}
	groups, products := stringSet(rules.Groups), stringSet(rules.ProductIDs)
	items, menuGroups, days := map[string]Entry{}, map[string]Entry{}, map[string]Entry{}
	orders := map[string]struct{}{}
	dessertOrders := map[string]Decimal{}
	startDate := time.Date(period.From.Year(), period.From.Month(), period.From.Day(), 0, 0, 0, 0, period.From.Location())
	localEnd := period.To.In(period.From.Location())
	endDate := time.Date(localEnd.Year(), localEnd.Month(), localEnd.Day(), 0, 0, 0, 0, period.From.Location())
	if !localEnd.Equal(endDate) {
		endDate = endDate.AddDate(0, 0, 1)
	}
	for _, row := range dataset.Rows {
		if len(rules.ProductIDs) > 0 && row.ProductID == "" {
			return Report{}, fmt.Errorf("configured dessert product IDs require product identities in sales data: %w", ErrInvalidDataset)
		}
		if row.Date == "" || row.Name == "" {
			return Report{}, fmt.Errorf("missing sales row date or name: %w", ErrInvalidDataset)
		}
		day := row.Date
		date, err := time.ParseInLocation("2006-01-02", day, period.From.Location())
		if err != nil {
			return Report{}, fmt.Errorf("invalid sales row date: %w", ErrInvalidDataset)
		}
		if date.Before(startDate) || !date.Before(endDate) {
			return Report{}, fmt.Errorf("sales row date outside requested period: %w", ErrInvalidDataset)
		}
		report.Revenue = report.Revenue.Add(row.Revenue)
		report.Quantity = report.Quantity.Add(row.Quantity)
		key := row.ProductID
		if key == "" {
			key = row.Group + "\x00" + row.Name
		}
		addEntry(items, key, Entry{ID: row.ProductID, Name: row.Name, Group: row.Group}, row)
		addEntry(menuGroups, row.Group, Entry{Name: row.Group}, row)
		addEntry(days, day, Entry{Name: day}, row)
		if dataset.OrderIDsAvailable {
			if row.OrderID == "" {
				return Report{}, fmt.Errorf("missing order identity: %w", ErrInvalidDataset)
			}
			orders[row.OrderID] = struct{}{}
		}
		_, groupDessert := groups[row.Group]
		_, productDessert := products[row.ProductID]
		if groupDessert || (row.ProductID != "" && productDessert) {
			report.DessertQuantity = report.DessertQuantity.Add(row.Quantity)
			report.DessertRevenue = report.DessertRevenue.Add(row.Revenue)
			if dataset.OrderIDsAvailable {
				dessertOrders[row.OrderID] = dessertOrders[row.OrderID].Add(row.Quantity)
			}
		}
	}
	if dataset.ReceiptCount != nil {
		count := *dataset.ReceiptCount
		report.Receipts = &count
	}
	if dataset.OrderIDsAvailable {
		count := len(orders)
		if report.Receipts != nil && *report.Receipts != count {
			return Report{}, fmt.Errorf("receipt aggregate disagrees with distinct order identities: %w", ErrInvalidDataset)
		}
		report.Receipts = &count
		dessertCount := 0
		for _, quantity := range dessertOrders {
			if quantity.Cmp(Decimal{}) > 0 {
				dessertCount++
			}
		}
		without := count - dessertCount
		report.DessertReceipts = &dessertCount
		report.WithoutDessertReceipts = &without
		if count > 0 {
			share, err := DecimalFromInt(int64(dessertCount * 100)).Div(DecimalFromInt(int64(count)))
			if err != nil {
				return Report{}, fmt.Errorf("calculate dessert share: %w", err)
			}
			report.DessertShare = &share
		}
	} else {
		report.Warnings = append(report.Warnings, "Чеки с десертами и без десертов недоступны: iiko не предоставила идентификатор конкретного заказа. Суммировать UniqOrderId по позициям нельзя.")
	}
	if report.Receipts != nil {
		if *report.Receipts == 0 && !report.Revenue.IsZero() {
			return Report{}, fmt.Errorf("nonzero revenue with zero receipts: %w", ErrInvalidDataset)
		}
		if *report.Receipts > 0 {
			average, err := report.Revenue.Div(DecimalFromInt(int64(*report.Receipts)))
			if err != nil {
				return Report{}, fmt.Errorf("calculate average receipt: %w", err)
			}
			report.Average = average
		}
	}
	report.Items = entries(items)
	report.Groups = entries(menuGroups)
	report.Daily = entries(days)
	sort.Slice(report.Daily, func(i, j int) bool { return report.Daily[i].Name < report.Daily[j].Name })
	return report, nil
}
func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}
func addEntry(values map[string]Entry, key string, identity Entry, row Row) {
	entry, exists := values[key]
	if !exists {
		entry = identity
	}
	entry.Quantity = entry.Quantity.Add(row.Quantity)
	entry.Revenue = entry.Revenue.Add(row.Revenue)
	values[key] = entry
}
func entries(values map[string]Entry) []Entry {
	result := make([]Entry, 0, len(values))
	for _, entry := range values {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if cmp := result[i].Revenue.Cmp(result[j].Revenue); cmp != 0 {
			return cmp > 0
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		if result[i].Group != result[j].Group {
			return result[i].Group < result[j].Group
		}
		return result[i].ID < result[j].ID
	})
	return result
}
