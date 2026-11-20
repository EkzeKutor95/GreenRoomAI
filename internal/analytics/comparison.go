package analytics

import "strings"

type Comparison struct {
	Current, Previous                                                                 Report
	RevenueChange, ReceiptChange, AverageChange, QuantityChange, DessertRevenueChange *Decimal
}

// PercentChange is undefined when the previous value is zero. A negative
// previous value uses its absolute magnitude as the comparison baseline.
func PercentChange(current, previous Decimal) *Decimal {
	if previous.IsZero() {
		return nil
	}
	baseline := previous
	if baseline.Cmp(Decimal{}) < 0 {
		baseline = Decimal{}.Sub(baseline)
	}
	change, err := current.Sub(previous).Mul(DecimalFromInt(100)).Div(baseline)
	if err != nil {
		return nil
	} // baseline is proven nonzero above.
	return &change
}
func Compare(current, previous Report) Comparison {
	result := Comparison{Current: current, Previous: previous, RevenueChange: PercentChange(current.Revenue, previous.Revenue), QuantityChange: PercentChange(current.Quantity, previous.Quantity), DessertRevenueChange: PercentChange(current.DessertRevenue, previous.DessertRevenue)}
	if current.Receipts != nil && previous.Receipts != nil {
		result.ReceiptChange = PercentChange(DecimalFromInt(int64(*current.Receipts)), DecimalFromInt(int64(*previous.Receipts)))
		if *current.Receipts > 0 && *previous.Receipts > 0 {
			result.AverageChange = PercentChange(current.Average, previous.Average)
		}
	}
	return result
}
func FormatComparison(comparison Comparison) string {
	var b strings.Builder
	b.WriteString("Сравнение периодов\nТекущий: " + comparison.Current.Period.String() + "\nБазовый: " + comparison.Previous.Period.String() + "\n\n")
	b.WriteString("Выручка: " + comparison.Current.Revenue.Fixed(2) + " ₽ / " + comparison.Previous.Revenue.Fixed(2) + " ₽; " + changeText(comparison.RevenueChange) + "\n")
	b.WriteString("Чеки: " + countText(comparison.Current.Receipts) + " / " + countText(comparison.Previous.Receipts) + "; " + changeText(comparison.ReceiptChange) + "\n")
	b.WriteString("Средний чек: " + averageText(comparison.Current) + " / " + averageText(comparison.Previous) + "; " + changeText(comparison.AverageChange) + "\n")
	b.WriteString("Единицы: " + comparison.Current.Quantity.String() + " / " + comparison.Previous.Quantity.String() + "; " + changeText(comparison.QuantityChange) + "\n")
	b.WriteString("Выручка десертов: " + comparison.Current.DessertRevenue.Fixed(2) + " ₽ / " + comparison.Previous.DessertRevenue.Fixed(2) + " ₽; " + changeText(comparison.DessertRevenueChange) + "\n")
	b.WriteString("Десерты, единицы: " + comparison.Current.DessertQuantity.String() + " / " + comparison.Previous.DessertQuantity.String() + "\n")
	b.WriteString("\nТОП текущего периода:\n")
	writeEntries(&b, comparison.Current.Items, 10)
	b.WriteString("\nТОП базового периода:\n")
	writeEntries(&b, comparison.Previous.Items, 10)
	seenWarnings := map[string]bool{}
	for _, report := range []Report{comparison.Current, comparison.Previous} {
		for _, warning := range report.Warnings {
			if !seenWarnings[warning] {
				b.WriteString("\n⚠ " + warning + "\n")
				seenWarnings[warning] = true
			}
		}
	}
	return b.String()
}
func changeText(change *Decimal) string {
	if change == nil {
		return "изменение не определено (нулевая база или нет данных)"
	}
	return change.Fixed(2) + "%"
}
