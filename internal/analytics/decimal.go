package analytics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Decimal is an immutable exact number. Its zero value represents zero.
// Sums and products never round; rounding occurs only when displaying a value.
type Decimal struct{ value *big.Rat }

var decimalPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

func ParseDecimal(value string) (Decimal, error) {
	if len(value) > 2048 || !decimalPattern.MatchString(value) {
		return Decimal{}, ErrInvalidDecimal
	}
	if at := strings.IndexAny(value, "eE"); at >= 0 {
		exponent, err := strconv.Atoi(value[at+1:])
		if err != nil || exponent < -1000 || exponent > 1000 {
			return Decimal{}, ErrInvalidDecimal
		}
	}
	r, ok := new(big.Rat).SetString(value)
	if !ok {
		return Decimal{}, ErrInvalidDecimal
	}
	return Decimal{value: r}, nil
}

func DecimalFromInt(value int64) Decimal { return Decimal{value: new(big.Rat).SetInt64(value)} }
func (d Decimal) rat() *big.Rat {
	if d.value == nil {
		return new(big.Rat)
	}
	return new(big.Rat).Set(d.value)
}
func (d Decimal) Add(other Decimal) Decimal {
	return Decimal{value: new(big.Rat).Add(d.rat(), other.rat())}
}
func (d Decimal) Sub(other Decimal) Decimal {
	return Decimal{value: new(big.Rat).Sub(d.rat(), other.rat())}
}
func (d Decimal) Mul(other Decimal) Decimal {
	return Decimal{value: new(big.Rat).Mul(d.rat(), other.rat())}
}
func (d Decimal) Div(other Decimal) (Decimal, error) {
	if other.IsZero() {
		return Decimal{}, ErrDivisionByZero
	}
	return Decimal{value: new(big.Rat).Quo(d.rat(), other.rat())}, nil
}
func (d Decimal) Cmp(other Decimal) int { return d.rat().Cmp(other.rat()) }
func (d Decimal) IsZero() bool          { return d.rat().Sign() == 0 }

// Fixed uses round-half-away-from-zero, including for signed return amounts.
func (d Decimal) Fixed(places int) string {
	if places < 0 {
		places = 0
	}
	r := d.rat()
	negative := r.Sign() < 0
	numerator := new(big.Int).Abs(r.Num())
	numerator.Mul(numerator, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, r.Denom(), remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(r.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	text := quotient.String()
	if places > 0 {
		for len(text) <= places {
			text = "0" + text
		}
		text = text[:len(text)-places] + "." + text[len(text)-places:]
	}
	if negative && quotient.Sign() != 0 {
		text = "-" + text
	}
	return text
}

func (d Decimal) String() string {
	r := d.rat()
	denominator := new(big.Int).Set(r.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	counts := [2]int{}
	for i, factor := range []*big.Int{two, five} {
		for new(big.Int).Mod(denominator, factor).Sign() == 0 {
			denominator.Quo(denominator, factor)
			counts[i]++
		}
	}
	places := counts[0]
	if counts[1] > places {
		places = counts[1]
	}
	// Ratios such as the average receipt can repeat; six display digits suffice,
	// while the value retained for comparisons remains exact.
	if denominator.Cmp(big.NewInt(1)) != 0 {
		places = 6
	}
	value := d.Fixed(places)
	if strings.Contains(value, ".") {
		value = strings.TrimRight(strings.TrimRight(value, "0"), ".")
	}
	return value
}
func (d Decimal) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *Decimal) UnmarshalJSON(data []byte) error {
	var value string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("decode decimal: %w", err)
		}
	} else {
		value = string(bytes.TrimSpace(data))
	}
	parsed, err := ParseDecimal(value)
	if err != nil {
		return fmt.Errorf("decode decimal: %w", err)
	}
	*d = parsed
	return nil
}
