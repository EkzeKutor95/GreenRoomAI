package analytics_test

import (
	"bytes"
	"encoding/csv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
)

func decimal(value string) analytics.Decimal {
	d, err := analytics.ParseDecimal(value)
	Expect(err).NotTo(HaveOccurred())
	return d
}
func reportPeriod() analytics.Period {
	p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", time.FixedZone("MSK", 3*60*60))
	Expect(err).NotTo(HaveOccurred())
	return p
}
func row(order, name, group, quantity, revenue string) analytics.Row {
	return analytics.Row{Date: "2026-11-10", OrderID: order, Name: name, Group: group, Quantity: decimal(quantity), Revenue: decimal(revenue)}
}

var _ = Describe("Sales reporting", func() {
	When("one receipt includes several desserts and another has drinks only", func() {
		It("counts distinct receipts and retains fractional units and exact revenue", func() {
			dataset := analytics.Dataset{OrderIDsAvailable: true, Rows: []analytics.Row{row("a", "Торт", "Десерты", "3", "900.10"), row("a", "Кофе", "Напитки", "1", "200.20"), row("b", "Какао", "Напитки", "0.5", "100.10")}}
			report, err := analytics.Aggregate(reportPeriod(), dataset, analytics.DessertRules{Groups: []string{"Десерты", "Десерты из кухни"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Revenue.String()).To(Equal("1200.4"))
			Expect(report.Quantity.String()).To(Equal("4.5"))
			Expect(*report.Receipts).To(Equal(2))
			Expect(*report.DessertReceipts).To(Equal(1))
			Expect(*report.WithoutDessertReceipts).To(Equal(1))
			Expect(report.DessertShare.Fixed(2)).To(Equal("50.00"))
			Expect(report.Average.Fixed(2)).To(Equal("600.20"))
			Expect(report.Items).To(HaveLen(3))
			Expect(report.Groups).To(HaveLen(2))
			Expect(report.Daily).To(HaveLen(1))
		})
	})
	When("the API provides counts but no order identity", func() {
		It("uses a separate receipt total and marks dessert receipt metrics unavailable", func() {
			count := 4
			report, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{ReceiptCount: &count, Rows: []analytics.Row{row("", "Торт", "Десерты", "7", "1000")}}, analytics.DessertRules{Groups: []string{"Десерты"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(*report.Receipts).To(Equal(4))
			Expect(report.Average.String()).To(Equal("250"))
			Expect(report.DessertReceipts).To(BeNil())
			Expect(report.DessertShare).To(BeNil())
			Expect(report.Warnings).NotTo(BeEmpty())
		})
	})
	When("a dessert sale is fully returned", func() {
		It("deducts revenue and quantity and does not report dessert penetration for the net zero sale", func() {
			report, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{OrderIDsAvailable: true, Rows: []analytics.Row{row("a", "Торт", "Десерты", "1", "300"), row("a", "Торт", "Десерты", "-1", "-300"), row("a", "Кофе", "Напитки", "1", "200")}}, analytics.DessertRules{Groups: []string{"Десерты"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Revenue.String()).To(Equal("200"))
			Expect(report.Quantity.String()).To(Equal("1"))
			Expect(report.DessertRevenue.IsZero()).To(BeTrue())
			Expect(*report.DessertReceipts).To(Equal(0))
			Expect(*report.WithoutDessertReceipts).To(Equal(1))
		})
	})
	When("desserts are configured by product ID", func() {
		It("classifies the product independently of its menu group", func() {
			sale := row("a", "Пирог", "Кухня", "0.25", "99.99")
			sale.ProductID = "dessert-id"
			report, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{OrderIDsAvailable: true, Rows: []analytics.Row{sale}}, analytics.DessertRules{ProductIDs: []string{"dessert-id", "dessert-id"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(*report.DessertReceipts).To(Equal(1))
			Expect(report.DessertQuantity.String()).To(Equal("0.25"))
		})
	})
	When("receipt identities are missing or contradict the receipt aggregate", func() {
		It("rejects incomplete or contradictory financial data", func() {
			count := 2
			_, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{OrderIDsAvailable: true, ReceiptCount: &count, Rows: []analytics.Row{row("a", "Кофе", "Напитки", "1", "100")}}, analytics.DessertRules{})
			Expect(err).To(MatchError(ContainSubstring("receipt aggregate disagrees")))
			_, err = analytics.Aggregate(reportPeriod(), analytics.Dataset{OrderIDsAvailable: true, Rows: []analytics.Row{row("", "Кофе", "Напитки", "1", "100")}}, analytics.DessertRules{})
			Expect(err).To(MatchError(ContainSubstring("missing order identity")))
		})
	})
	When("there are no sales", func() {
		It("reports known zeros while leaving the undefined average and dessert share unavailable", func() {
			report, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{OrderIDsAvailable: true}, analytics.DessertRules{})
			Expect(err).NotTo(HaveOccurred())
			Expect(*report.Receipts).To(Equal(0))
			Expect(*report.DessertReceipts).To(Equal(0))
			Expect(report.DessertShare).To(BeNil())
			Expect(analytics.Format(report, 10)).To(ContainSubstring("Средний чек: недоступно"))
		})
	})
	When("CSV contains names beginning with a spreadsheet formula", func() {
		It("exports all source rows without formula execution or monetary rounding", func() {
			sale := row("a", "=IMPORTDATA(1)", "Десерты", "0.125", "0.001")
			report, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{OrderIDsAvailable: true, Rows: []analytics.Row{sale}}, analytics.DessertRules{})
			Expect(err).NotTo(HaveOccurred())
			data, err := analytics.CSV(report)
			Expect(err).NotTo(HaveOccurred())
			reader := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
			records, err := reader.ReadAll()
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(HaveLen(2))
			Expect(records[1][2]).To(Equal("'=IMPORTDATA(1)"))
			Expect(records[1][5]).To(Equal("0.125"))
			Expect(records[1][6]).To(Equal("0.001"))
		})
	})
	When("two menu groups contain products with the same name", func() {
		It("does not merge distinct unnamed product identities across groups", func() {
			report, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{Rows: []analytics.Row{row("a", "Латте", "Кофе", "1", "200"), row("b", "Латте", "Спецменю", "1", "300")}}, analytics.DessertRules{})
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Items).To(HaveLen(2))
		})
	})
	When("financial inputs contain malformed dates or impossible receipt totals", func() {
		It("rejects those inputs before presenting an apparently valid report", func() {
			sale := row("a", "Кофе", "Напитки", "1", "100")
			sale.Date = "wrong"
			_, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{Rows: []analytics.Row{sale}}, analytics.DessertRules{})
			Expect(err).To(HaveOccurred())
			count := 0
			_, err = analytics.Aggregate(reportPeriod(), analytics.Dataset{ReceiptCount: &count, Rows: []analytics.Row{row("a", "Кофе", "Напитки", "1", "100")}}, analytics.DessertRules{})
			Expect(err).To(HaveOccurred())
		})
	})
})

var _ = Describe("Sales date contract", func() {
	When("source rows have an invalid suffix or lie outside the requested dates", func() {
		It("rejects them without silently truncating or expanding the period", func() {
			for _, date := range []string{"2026-11-10junk", "2026-11-10T00:00:00", "2026-11-09", "2026-11-11"} {
				sale := row("a", "Кофе", "Напитки", "1", "100")
				sale.Date = date
				_, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{Rows: []analytics.Row{sale}}, analytics.DessertRules{})
				Expect(err).To(HaveOccurred())
			}
		})
	})
	When("a source supports an exact intraday interval", func() {
		It("accepts its calendar date while retaining the precise requested interval", func() {
			p := reportPeriod()
			p.From = p.From.Add(7 * time.Hour)
			p.To = p.From.Add(11 * time.Hour)
			report, err := analytics.Aggregate(p, analytics.Dataset{Rows: []analytics.Row{row("a", "Кофе", "Напитки", "1", "100")}}, analytics.DessertRules{})
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Period).To(Equal(p))
		})
	})
})

var _ = Describe("Dessert identity completeness", func() {
	When("configured dessert product IDs are not present in the sales source", func() {
		It("rejects incomplete classification instead of showing misleading category-only totals", func() {
			_, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{Rows: []analytics.Row{row("a", "Кофе", "Напитки", "1", "100")}}, analytics.DessertRules{ProductIDs: []string{"dessert-id"}})
			Expect(err).To(MatchError(ContainSubstring("configured dessert product IDs require product identities")))
		})
	})
})

var _ = Describe("Calendar periods", func() {
	loc := time.FixedZone("MSK", 3*60*60)
	When("a custom interval includes a leap day", func() {
		It("includes both selected dates and uses the venue timezone", func() {
			p, err := analytics.NewPeriod("2024-02-28", "2024-02-29", loc)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.From.Hour()).To(Equal(0))
			Expect(p.To.Format("2006-01-02")).To(Equal("2024-03-01"))
			Expect(p.To.Sub(p.From)).To(Equal(48 * time.Hour))
			_, offset := p.From.Zone()
			Expect(offset).To(Equal(3 * 60 * 60))
		})
	})
	When("a scheduled evening crosses midnight", func() {
		It("attributes the schedule to its start date and ends on the next date", func() {
			date := time.Date(2026, 11, 10, 0, 0, 0, 0, loc)
			p := analytics.ScheduledShift(date, true, loc)
			Expect(p.From.Format("2006-01-02 15:04")).To(Equal("2026-11-10 18:00"))
			Expect(p.To.Format("2006-01-02 15:04")).To(Equal("2026-11-11 01:00"))
		})
	})
	When("a month-to-date comparison falls on March 31", func() {
		It("compares only completed dates and caps the February interval at its month boundary", func() {
			now := time.Date(2026, 3, 31, 15, 30, 0, 0, loc)
			current, previous, err := analytics.ComparablePeriods("month_to_date", now, loc)
			Expect(err).NotTo(HaveOccurred())
			Expect(current.To.Format("2006-01-02 15:04")).To(Equal("2026-03-31 00:00"))
			Expect(previous.From.Format("2006-01-02")).To(Equal("2026-02-01"))
			Expect(previous.To.Format("2006-01-02 15:04")).To(Equal("2026-03-01 00:00"))
		})
	})
	When("seven completed days are compared", func() {
		It("uses adjacent disjoint seven-day ranges ending today at midnight", func() {
			now := time.Date(2026, 11, 13, 17, 0, 0, 0, loc)
			current, previous, err := analytics.ComparablePeriods("7d", now, loc)
			Expect(err).NotTo(HaveOccurred())
			Expect(current.From.Format("2006-01-02")).To(Equal("2026-11-06"))
			Expect(current.To.Format("2006-01-02")).To(Equal("2026-11-13"))
			Expect(previous.To).To(Equal(current.From))
			Expect(previous.From.Format("2006-01-02")).To(Equal("2026-10-30"))
		})
	})
	When("the range is reversed, too long, or malformed", func() {
		It("rejects it rather than silently normalizing the dates", func() {
			for _, dates := range [][2]string{{"2026-11-11", "2026-11-10"}, {"2024-01-01", "2026-01-01"}, {"2026-02-30", "2026-03-01"}} {
				_, err := analytics.NewPeriod(dates[0], dates[1], loc)
				Expect(err).To(HaveOccurred())
			}
		})
	})
	When("a preset includes today's sales", func() {
		It("selects seven venue-local calendar dates", func() {
			p, err := analytics.Preset("7d", time.Date(2026, 11, 13, 15, 0, 0, 0, loc), loc)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.From.Format("2006-01-02")).To(Equal("2026-11-07"))
			Expect(p.To.Format("2006-01-02")).To(Equal("2026-11-14"))
		})
	})
})

var _ = Describe("Period comparisons", func() {
	When("source reports carry refund or capability limitations", func() {
		It("keeps those limitations visible in comparisons without duplicates", func() {
			current := analytics.Report{Warnings: []string{"Refund policy must be verified", "Order identity unavailable"}}
			previous := analytics.Report{Warnings: []string{"Refund policy must be verified"}}
			text := analytics.FormatComparison(analytics.Compare(current, previous))
			Expect(strings.Count(text, "Refund policy must be verified")).To(Equal(1))
			Expect(text).To(ContainSubstring("Order identity unavailable"))
		})
	})

	When("the previous period has no sales", func() {
		It("marks percentage changes undefined instead of inventing growth", func() {
			c := analytics.Compare(analytics.Report{Revenue: decimal("100")}, analytics.Report{})
			Expect(c.RevenueChange).To(BeNil())
			Expect(analytics.FormatComparison(c)).To(ContainSubstring("изменение не определено"))
		})
	})
	When("the prior revenue is positive", func() {
		It("computes exact percentage changes in Go", func() {
			c := analytics.Compare(analytics.Report{Revenue: decimal("120.12")}, analytics.Report{Revenue: decimal("100.1")})
			Expect(c.RevenueChange.String()).To(Equal("20"))
			Expect(strings.Contains(analytics.FormatComparison(c), "20.00%")).To(BeTrue())
		})
	})
})
