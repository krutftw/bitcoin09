package mobilewallet

import (
	"errors"
	"strconv"
	"strings"

	"github.com/krutftw/bitcoin09/core"
)

func parseAmount(text string, allowZero bool) (int64, error) {
	if text == "" || len(text) > 32 {
		return 0, errors.New("invalid decimal length")
	}
	for _, character := range []byte(text) {
		if (character < '0' || character > '9') && character != '.' {
			return 0, errors.New("amount must be a plain decimal")
		}
	}
	parts := strings.Split(text, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && (parts[1] == "" || len(parts[1]) > 8)) {
		return 0, errors.New("amount must have up to eight decimal places")
	}
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || whole > 21_000_000 {
		return 0, errors.New("amount exceeds the maximum supply")
	}
	var fraction uint64
	if len(parts) == 2 {
		fraction, err = strconv.ParseUint(parts[1]+strings.Repeat("0", 8-len(parts[1])), 10, 64)
		if err != nil {
			return 0, errors.New("invalid fractional amount")
		}
	}
	if whole == 21_000_000 && fraction != 0 {
		return 0, errors.New("amount exceeds the maximum supply")
	}
	units := whole*uint64(core.UnitsPerCoin) + fraction
	if units > uint64(core.MaxMoneyUnits) || (!allowZero && units == 0) {
		return 0, errors.New("amount is out of range")
	}
	return int64(units), nil
}
