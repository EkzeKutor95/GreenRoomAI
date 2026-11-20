package analytics

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func Format(report Report, top int) string {
	return formatReport(report, top, "GREEN ROOM · Продажи\n"+report.Period.String(), true)
}

// FormatDaily renders one venue-local accounting date without showing a second date.
func FormatDaily(report Report) (string, error) {
	if err := report.Period.Validate(); err != nil {
		return "", err
	}
	from := report.Period.From
	midnight := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	if !from.Equal(midnight) || !report.Period.To.Equal(midnight.AddDate(0, 0, 1)) {
		return "", fmt.Errorf("daily report requires one complete local calendar date: %w", ErrInvalidPeriod)
	}
	return formatReport(report, 0, "GREEN ROOM · Отчёт за "+from.Format("02.01.2006"), false), nil
}

func formatReport(report Report, top int, caption string, includeDaily bool) string {
	var b strings.Builder
	b.WriteString(caption + "\n\n")
	b.WriteString("Выручка: " + report.Revenue.Fixed(2) + " ₽\nЧеки: " + countText(report.Receipts) + "\nПродано единиц: " + report.Quantity.String() + "\nСредний чек: " + averageText(report) + "\n")
	b.WriteString("\nДесерты:\nЧеки с десертами: " + countText(report.DessertReceipts) + "\nБез десертов: " + countText(report.WithoutDessertReceipts) + "\nДоля: ")
	if report.DessertShare == nil {
		b.WriteString("недоступно")
	} else {
		b.WriteString(report.DessertShare.Fixed(2) + "%")
	}
	b.WriteString("\nЕдиницы: " + report.DessertQuantity.String() + "\nВыручка: " + report.DessertRevenue.Fixed(2) + " ₽\n\nПо группам:\n")
	writeEntries(&b, report.Groups, 0)
	if top > 0 {
		b.WriteString("\nТОП-" + strconv.Itoa(top) + " позиций:\n")
	} else {
		b.WriteString("\nВсе позиции:\n")
	}
	writeEntries(&b, report.Items, top)
	if includeDaily {
		b.WriteString("\nВыручка по дням (OpenDate.Typed):\n")
		writeEntries(&b, report.Daily, 0)
	}
	for _, warning := range report.Warnings {
		b.WriteString("\n⚠ " + warning + "\n")
	}
	return b.String()
}
func countText(count *int) string {
	if count == nil {
		return "недоступно"
	}
	return strconv.Itoa(*count)
}
func averageText(report Report) string {
	if report.Receipts == nil || *report.Receipts == 0 {
		return "недоступно"
	}
	return report.Average.Fixed(2) + " ₽"
}
func writeEntries(b *strings.Builder, entries []Entry, top int) {
	if len(entries) == 0 {
		b.WriteString("Нет продаж.\n")
		return
	}
	for i, entry := range entries {
		if top > 0 && i >= top {
			break
		}
		name := entry.Name
		if entry.Group != "" {
			name += " (" + entry.Group + ")"
		}
		if name == "" {
			name = "(группа не указана iiko)"
		}
		fmt.Fprintf(b, "%d. %s: %s ед. · %s ₽\n", i+1, name, entry.Quantity.String(), entry.Revenue.Fixed(2))
	}
}

// CSV contains every source OLAP row and exact, unrounded amounts. Text cells
// are protected against spreadsheet formula execution; monetary columns stay numeric.
func CSV(report Report) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("\ufeff")
	writer := csv.NewWriter(&b)
	if err := writer.Write([]string{"Дата", "Группа", "Позиция", "ID товара", "ID заказа", "Количество", "Выручка после скидок"}); err != nil {
		return nil, fmt.Errorf("write CSV header: %w", err)
	}
	for _, row := range report.Rows {
		if err := writer.Write([]string{safeCell(row.Date), safeCell(row.Group), safeCell(row.Name), safeCell(row.ProductID), safeCell(row.OrderID), row.Quantity.String(), row.Revenue.String()}); err != nil {
			return nil, fmt.Errorf("write CSV row: %w", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("flush CSV: %w", err)
	}
	return b.Bytes(), nil
}
func safeCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}
