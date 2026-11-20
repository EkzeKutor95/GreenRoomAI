package analytics_test

import (
	"encoding/json"
	"greenroom-ai/internal/analytics"
	"strings"
	"testing"
)

func TestExactDecimalArithmetic(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct{ name, left, right, want string }{{"tenths remain exact", "0.1", "0.2", "0.3"}, {"large sales do not overflow", "999999999999999999999.99", "0.01", "1000000000000000000000"}, {"signed return offsets sales", "100.025", "-100", "0.025"}, {"scientific JSON notation", "1e-3", "0.002", "0.003"}} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			left, err := analytics.ParseDecimal(scenario.left)
			if err != nil {
				t.Fatal(err)
			}
			right, err := analytics.ParseDecimal(scenario.right)
			if err != nil {
				t.Fatal(err)
			}
			if got := left.Add(right).String(); got != scenario.want {
				t.Fatalf("want %s, got %s", scenario.want, got)
			}
			if left.String() != decimalString(t, scenario.left) {
				t.Fatal("addition mutated its operand")
			}
		})
	}
}
func decimalString(t *testing.T, value string) string {
	t.Helper()
	d, err := analytics.ParseDecimal(value)
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
}
func TestDecimalDisplayRounding(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct{ input, want string }{{"1.005", "1.01"}, {"-1.005", "-1.01"}, {"-0.001", "0.00"}, {"0", "0.00"}} {
		d, err := analytics.ParseDecimal(scenario.input)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Fixed(2); got != scenario.want {
			t.Fatalf("%s: want %s, got %s", scenario.input, scenario.want, got)
		}
	}
}
func TestMalformedFinancialNumbersAreRejected(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", " 1", "01", "1/3", "NaN", "null", "1e999999", "+2", "1,2"} {
		if _, err := analytics.ParseDecimal(input); err == nil {
			t.Fatalf("accepted malformed number %q", input)
		}
	}
	if _, err := (analytics.Decimal{}).Div(analytics.Decimal{}); err == nil {
		t.Fatal("accepted division by zero")
	}
}
func TestJSONNumbersNeverPassThroughFloat(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`9007199254740993.01`, `"9007199254740993.01"`} {
		var d analytics.Decimal
		if err := json.Unmarshal([]byte(input), &d); err != nil {
			t.Fatal(err)
		}
		if d.String() != "9007199254740993.01" {
			t.Fatal(d.String())
		}
	}
}

func TestVerySmallAmountsRemainExactInExports(t *testing.T) {
	t.Parallel()
	value := "0." + strings.Repeat("0", 1001) + "1"
	d, err := analytics.ParseDecimal(value)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.String(); got != value {
		t.Fatalf("lost subunit precision: got %q", got)
	}
}
